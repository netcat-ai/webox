package wechat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/netcat-ai/webox/internal/signedpayload"
	"github.com/netcat-ai/webox/internal/wechatdb"
	"github.com/netcat-ai/webox/wecom"
)

const (
	maxPollLimit        = 500
	keyValidationPeriod = 30 * time.Second
	agentRemarkPrefix   = "webox."
)

type InitializationState int

const (
	WaitingForLogin InitializationState = iota
	Ready
)

type State struct {
	stateDir            string
	keyFile             string
	cursorKey           string
	remarkFilterEnabled bool

	initialized      atomic.Bool
	lastValidationAt atomic.Int64
	dbMu             sync.Mutex
	database         *wechatdb.Store
	wxid             string
}

type keyFile struct {
	WXID  string            `json:"wxid"`
	DBDir string            `json:"dbDir"`
	Keys  map[string]string `json:"keys"`
}

type dbCursor struct {
	StartedAt int64                     `json:"started_at"`
	Positions wechatdb.MessagePositions `json:"positions"`
}

type PollResult struct {
	AccountID string
	Cursor    string
	Messages  []wecom.Message
	Skipped   []wechatdb.SkippedMessage
}

type UserInfo struct {
	AccountID string
	WeChatID  string
	Nickname  string
	AvatarURL string
}

func filterMessagesByRemarkPrefix(
	messages []wecom.Message,
	lookup func(string) (string, error),
) ([]wecom.Message, error) {
	filtered := make([]wecom.Message, 0, len(messages))
	remarks := make(map[string]string)
	for _, message := range messages {
		roomID := strings.TrimSpace(message.RoomID)
		if roomID == "" {
			continue
		}
		remark, found := remarks[roomID]
		if !found {
			var err error
			remark, err = lookup(roomID)
			if err != nil {
				return nil, err
			}
			remarks[roomID] = remark
		}
		if strings.HasPrefix(strings.TrimSpace(remark), agentRemarkPrefix) {
			filtered = append(filtered, message)
		}
	}
	return filtered, nil
}

func New(stateDir, cursorKey string, remarkFilterEnabled bool) *State {
	return &State{
		stateDir:            stateDir,
		keyFile:             filepath.Join(stateDir, "wechat.key"),
		cursorKey:           cursorKey,
		remarkFilterEnabled: remarkFilterEnabled,
	}
}

func (state *State) applyRemarkFilter(
	messages []wecom.Message,
	lookup func(string) (string, error),
) ([]wecom.Message, error) {
	if !state.remarkFilterEnabled {
		return messages, nil
	}
	return filterMessagesByRemarkPrefix(messages, lookup)
}

func (state *State) EnsureStateDir() error {
	return os.MkdirAll(state.stateDir, 0o700)
}

func (state *State) IsInitialized() bool {
	return state.initialized.Load()
}

func (state *State) AccountID() (string, error) {
	state.dbMu.Lock()
	defer state.dbMu.Unlock()
	database, wxid, err := state.readyDatabase()
	if err != nil {
		return "", err
	}
	info, err := state.currentAccountInfo(database, wxid)
	return info.AccountID, err
}

func (state *State) UserInfo() (UserInfo, error) {
	state.dbMu.Lock()
	defer state.dbMu.Unlock()
	database, wxid, err := state.readyDatabase()
	if err != nil {
		return UserInfo{}, err
	}
	info, err := state.currentAccountInfo(database, wxid)
	if err != nil {
		return UserInfo{}, err
	}
	return UserInfo{
		AccountID: info.AccountID,
		WeChatID:  info.WeChatID,
		Nickname:  info.Nickname,
		AvatarURL: info.AvatarURL,
	}, nil
}

func (state *State) currentAccountInfo(database *wechatdb.Store, username string) (wechatdb.AccountInfo, error) {
	info, err := database.AccountInfoFor(username)
	if err != nil {
		return wechatdb.AccountInfo{}, state.dbError("read current WeChat account", err)
	}
	if strings.TrimSpace(info.AccountID) == "" || strings.TrimSpace(info.WeChatID) == "" {
		return wechatdb.AccountInfo{}, errors.New("current WeChat account identity is unavailable")
	}
	return info, nil
}

func (state *State) ValidatePollCursor(rawCursor string) error {
	if strings.TrimSpace(rawCursor) == "" {
		return nil
	}
	var cursor dbCursor
	if err := signedpayload.Decode(state.cursorKey, rawCursor, &cursor); err != nil {
		return fmt.Errorf("decode get_updates_buf: %w", err)
	}
	if cursor.StartedAt <= 0 {
		return errors.New("unsupported get_updates_buf")
	}
	return nil
}

func (state *State) InitializeIfReady() (InitializationState, error) {
	ready, known := wechatMainWindowReady()
	if !known || !ready {
		state.initialized.Store(false)
		return WaitingForLogin, nil
	}
	state.dbMu.Lock()
	defer state.dbMu.Unlock()

	activeDBDir := wechatdb.DetectStorage()
	if activeDBDir == "" {
		return WaitingForLogin, errors.New("wechat db_storage directory not found")
	}
	activeWXID := wechatdb.AccountIDFromDBDir(activeDBDir)
	if activeWXID == "" {
		return WaitingForLogin, errors.New("cannot identify active WeChat account")
	}
	material, materialErr := state.readKey()
	if materialErr == nil && (material.WXID != activeWXID || wechatdb.AccountIDFromDBDir(material.DBDir) != activeWXID) {
		materialErr = errors.New("stored WeChat database key belongs to another account")
	}
	if state.IsInitialized() && state.database != nil && materialErr == nil {
		if time.Since(time.Unix(state.lastValidationAt.Load(), 0)) < keyValidationPeriod {
			return Ready, nil
		}
		if _, err := state.database.CurrentSessionState(); err == nil {
			state.lastValidationAt.Store(time.Now().Unix())
			return Ready, nil
		}
	}
	state.initialized.Store(false)
	var database *wechatdb.Store
	if materialErr == nil {
		database, materialErr = openDatabase(material)
	}
	if materialErr != nil {
		init, err := wechatdb.InitFromMemory()
		if err != nil {
			return WaitingForLogin, fmt.Errorf("extract wechat message keys during automatic initialization: %w", err)
		}
		material = keyFile{WXID: init.WXID, DBDir: init.DBDir, Keys: init.Keys}
		database, err = openDatabase(material)
		if err != nil {
			return WaitingForLogin, fmt.Errorf("validate wechat database keys: %w", err)
		}
		if err := state.writeKey(material); err != nil {
			_ = database.Close()
			return WaitingForLogin, err
		}
	}
	if state.database != nil && state.database != database {
		_ = state.database.Close()
	}
	state.database = database
	state.wxid = material.WXID
	state.initialized.Store(true)
	state.lastValidationAt.Store(time.Now().Unix())
	return Ready, nil
}

func (state *State) MarkUninitialized() {
	state.initialized.Store(false)
}

func (state *State) ClickSavedAccountLogin() (bool, error) {
	window := wechatLoginWindow()
	if window == "" {
		return false, nil
	}
	if output, err := exec.Command("xdotool", "mousemove", "--window", window, "140", "290", "click", "1").CombinedOutput(); err != nil {
		return false, fmt.Errorf("click saved-account login button: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return true, nil
}

func (state *State) RefreshLoginQRCode() (bool, error) {
	window := wechatLoginWindow()
	if window == "" {
		return false, nil
	}
	if output, err := exec.Command("xdotool", "mousemove", "--window", window, "140", "130", "click", "1").CombinedOutput(); err != nil {
		return false, fmt.Errorf("click expired QR refresh area: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return true, nil
}

func (state *State) DismissPostLoginOverlay() (bool, error) {
	window := wechatMainWindow()
	if window == "" {
		return false, nil
	}
	if output, err := exec.Command("xdotool", "windowactivate", "--sync", window, "key", "--clearmodifiers", "Escape").CombinedOutput(); err != nil {
		return false, fmt.Errorf("run xdotool for post-login overlay: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return true, nil
}

func (state *State) PollMessages(rawCursor string, limit int) (PollResult, error) {
	state.dbMu.Lock()
	defer state.dbMu.Unlock()
	database, wxid, err := state.readyDatabase()
	if err != nil {
		return PollResult{}, err
	}
	account, err := state.currentAccountInfo(database, wxid)
	if err != nil {
		return PollResult{}, err
	}
	limit = min(max(limit, 1), maxPollLimit)
	var cursor dbCursor
	if strings.TrimSpace(rawCursor) == "" {
		cursor.StartedAt = time.Now().Unix()
		cursor.Positions, err = database.BaselinePositions(cursor.StartedAt)
		if err != nil {
			return PollResult{}, state.dbError("baseline WeChat messages", err)
		}
		encoded, err := state.encodeCursor(cursor)
		return PollResult{AccountID: account.AccountID, Cursor: encoded, Messages: []wecom.Message{}}, err
	}
	if err := signedpayload.Decode(state.cursorKey, rawCursor, &cursor); err != nil {
		return PollResult{}, fmt.Errorf("decode get_updates_buf: %w", err)
	}
	if cursor.StartedAt <= 0 {
		return PollResult{}, errors.New("unsupported get_updates_buf")
	}
	data, err := database.PollNewMessages(cursor.Positions, cursor.StartedAt, limit)
	if err != nil {
		return PollResult{}, state.dbError("poll WeChat messages", err)
	}
	sort.SliceStable(data.Messages, func(i, j int) bool {
		left, right := messageOrder(data.Messages[i]), messageOrder(data.Messages[j])
		if left.timestamp != right.timestamp {
			return left.timestamp < right.timestamp
		}
		if left.localID != right.localID {
			return left.localID < right.localID
		}
		return left.room < right.room
	})
	messages, err := state.applyRemarkFilter(data.Messages, database.ConversationRemark)
	if err != nil {
		return PollResult{}, state.dbError("filter WeChat messages by conversation remark", err)
	}
	cursor.Positions = data.NewState
	encoded, err := state.encodeCursor(cursor)
	if err != nil {
		return PollResult{}, err
	}
	return PollResult{AccountID: account.AccountID, Cursor: encoded, Messages: messages, Skipped: data.Skipped}, nil
}

func (state *State) ResolveRecipient(username string) (*wechatdb.Recipient, error) {
	state.dbMu.Lock()
	defer state.dbMu.Unlock()
	database, wxid, err := state.readyDatabase()
	if err != nil {
		return nil, err
	}
	recipient, err := database.ResolveRecipient(username, wxid)
	if err != nil {
		return nil, state.dbError("resolve WeChat recipient", err)
	}
	if recipient == nil {
		return nil, errors.New("recipient not found: target must be a WeChat internal id")
	}
	return recipient, nil
}

func (state *State) RoomMessagePositions(target string) (wechatdb.RoomMessagePositions, error) {
	state.dbMu.Lock()
	defer state.dbMu.Unlock()
	database, _, err := state.readyDatabase()
	if err != nil {
		return nil, err
	}
	positions, err := database.RoomMessagePositionsFor(target)
	if err != nil {
		return nil, state.dbError("read WeChat message positions", err)
	}
	return positions, nil
}

func (state *State) OutgoingItemsAfter(positions wechatdb.RoomMessagePositions, target string) ([]wechatdb.OutgoingItem, error) {
	state.dbMu.Lock()
	defer state.dbMu.Unlock()
	database, _, err := state.readyDatabase()
	if err != nil {
		return nil, err
	}
	items, err := database.OutgoingItemsAfter(target, positions)
	if err != nil {
		return nil, state.dbError("verify outgoing WeChat items", err)
	}
	return items, nil
}

func (state *State) ReadImage(roomID, messageID string) (*wechatdb.MediaFile, error) {
	state.dbMu.Lock()
	defer state.dbMu.Unlock()
	database, _, err := state.readyDatabase()
	if err != nil {
		return nil, err
	}
	media, err := database.ReadImage(roomID, messageID)
	if err != nil {
		return nil, state.dbError("read WeChat image", err)
	}
	return media, nil
}

func (state *State) ReadFile(roomID, messageID string) (*wechatdb.LocalFile, error) {
	state.dbMu.Lock()
	defer state.dbMu.Unlock()
	database, _, err := state.readyDatabase()
	if err != nil {
		return nil, err
	}
	file, err := database.ReadFile(roomID, messageID)
	if err != nil {
		return nil, state.dbError("read WeChat file", err)
	}
	return file, nil
}

func (state *State) readyDatabase() (*wechatdb.Store, string, error) {
	if !state.IsInitialized() {
		return nil, "", errors.New("wechat automatic initialization is not complete")
	}
	if state.database == nil {
		state.MarkUninitialized()
		return nil, "", errors.New("WeChat database is not open")
	}
	return state.database, state.wxid, nil
}

func openDatabase(material keyFile) (*wechatdb.Store, error) {
	database, err := wechatdb.Open(material.DBDir, material.Keys)
	if err != nil {
		return nil, err
	}
	if _, err := database.CurrentSessionState(); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}

func (state *State) readKey() (keyFile, error) {
	data, err := os.ReadFile(state.keyFile)
	if err != nil {
		return keyFile{}, fmt.Errorf("read key file %s: %w", state.keyFile, err)
	}
	var material keyFile
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&material); err != nil {
		return keyFile{}, err
	}
	if strings.TrimSpace(material.WXID) == "" || strings.TrimSpace(material.DBDir) == "" || len(material.Keys) == 0 {
		return keyFile{}, errors.New("wechat key file has no database keys")
	}
	if err := wechatdb.ValidateMessageDBKeys(material.DBDir, material.Keys); err != nil {
		return keyFile{}, err
	}
	return material, nil
}

func (state *State) writeKey(material keyFile) error {
	if err := state.EnsureStateDir(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(material, "", "  ")
	if err != nil {
		return err
	}
	tmp := state.keyFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, state.keyFile)
}

func (state *State) encodeCursor(cursor dbCursor) (string, error) {
	return signedpayload.Encode(state.cursorKey, cursor)
}

func (state *State) dbError(operation string, err error) error {
	wrapped := fmt.Errorf("%s: %w", operation, err)
	state.MarkUninitialized()
	return wrapped
}

type orderKey struct {
	timestamp int64
	localID   int64
	room      string
}

func messageOrder(message wecom.Message) orderKey {
	return orderKey{
		timestamp: message.MsgTime,
		localID:   message.Sequence,
		room:      message.RoomID,
	}
}

func wechatMainWindowReady() (bool, bool) {
	geometry, known := visibleWechatWindowGeometry()
	return mainWindowFromGeometry(geometry) != "", known
}

func wechatMainWindow() string {
	geometry, _ := visibleWechatWindowGeometry()
	return mainWindowFromGeometry(geometry)
}

func wechatLoginWindow() string {
	geometry, _ := visibleWechatWindowGeometry()
	return loginWindowFromGeometry(geometry)
}

func visibleWechatWindowGeometry() (string, bool) {
	if strings.TrimSpace(os.Getenv("DISPLAY")) == "" {
		return "", false
	}
	output, err := exec.Command(
		"xdotool", "search", "--onlyvisible", "--class", "wechat",
		"getwindowgeometry", "--shell", "%@",
	).Output()
	if err != nil {
		return "", true
	}
	return string(output), true
}

func loginWindowFromGeometry(output string) string {
	return windowFromGeometry(output, func(width, height int) bool { return width <= 400 && height <= 500 })
}

func mainWindowFromGeometry(output string) string {
	return windowFromGeometry(output, func(width, height int) bool { return width >= 700 && height >= 500 })
}

func windowFromGeometry(output string, matches func(int, int) bool) string {
	window, width := "", -1
	for _, line := range strings.Split(output, "\n") {
		if value, found := strings.CutPrefix(line, "WINDOW="); found {
			window, width = value, -1
			continue
		}
		if value, found := strings.CutPrefix(line, "WIDTH="); found {
			width, _ = strconv.Atoi(value)
			continue
		}
		if value, found := strings.CutPrefix(line, "HEIGHT="); found {
			height, err := strconv.Atoi(value)
			if err == nil && width >= 0 && matches(width, height) {
				return window
			}
			width = -1
		}
	}
	return ""
}

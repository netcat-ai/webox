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
	errorMu          sync.Mutex
	lastError        string
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
	Cursor   string
	Messages []map[string]any
}

func filterMessagesByRemarkPrefix(
	messages []map[string]any,
	lookup func(string) (string, error),
) ([]map[string]any, error) {
	filtered := make([]map[string]any, 0, len(messages))
	remarks := make(map[string]string)
	for _, message := range messages {
		roomID, ok := message["roomid"].(string)
		roomID = strings.TrimSpace(roomID)
		if !ok || roomID == "" {
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
	messages []map[string]any,
	lookup func(string) (string, error),
) ([]map[string]any, error) {
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
	if state.IsInitialized() && materialErr == nil && time.Since(time.Unix(state.lastValidationAt.Load(), 0)) < keyValidationPeriod {
		return Ready, nil
	}
	state.initialized.Store(false)
	if materialErr == nil {
		_, materialErr = wechatdb.CurrentSessionState(material.DBDir, material.Keys, state.cacheDir())
	}
	if materialErr != nil {
		init, err := wechatdb.InitFromMemory()
		if err != nil {
			return WaitingForLogin, fmt.Errorf("extract wechat message keys during automatic initialization: %w", err)
		}
		material = keyFile{WXID: init.WXID, DBDir: init.DBDir, Keys: init.Keys}
		if err := state.writeKey(material); err != nil {
			return WaitingForLogin, err
		}
		if _, err := wechatdb.CurrentSessionState(material.DBDir, material.Keys, state.cacheDir()); err != nil {
			return WaitingForLogin, fmt.Errorf("validate wechat database keys: %w", err)
		}
	}
	state.initialized.Store(true)
	state.lastValidationAt.Store(time.Now().Unix())
	state.setError("")
	return Ready, nil
}

func (state *State) RecordInitError(err error) {
	state.initialized.Store(false)
	state.setError(err.Error())
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
	material, err := state.readyMaterial()
	if err != nil {
		return PollResult{}, err
	}
	limit = min(max(limit, 1), maxPollLimit)
	var cursor dbCursor
	if strings.TrimSpace(rawCursor) == "" {
		cursor.StartedAt = time.Now().Unix()
		cursor.Positions, err = wechatdb.BaselinePositions(material.DBDir, material.Keys, state.cacheDir(), cursor.StartedAt)
		if err != nil {
			return PollResult{}, state.dbError("baseline WeChat messages", err)
		}
		encoded, err := state.encodeCursor(cursor)
		return PollResult{Cursor: encoded, Messages: []map[string]any{}}, err
	}
	if err := signedpayload.Decode(state.cursorKey, rawCursor, &cursor); err != nil {
		return PollResult{}, fmt.Errorf("decode get_updates_buf: %w", err)
	}
	if cursor.StartedAt <= 0 {
		return PollResult{}, errors.New("unsupported get_updates_buf")
	}
	data, err := wechatdb.PollNewMessages(material.DBDir, material.Keys, cursor.Positions, cursor.StartedAt, limit, state.cacheDir())
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
	messages, err := state.applyRemarkFilter(data.Messages, func(roomID string) (string, error) {
		return wechatdb.ConversationRemark(material.DBDir, material.Keys, state.cacheDir(), roomID)
	})
	if err != nil {
		return PollResult{}, state.dbError("filter WeChat messages by conversation remark", err)
	}
	cursor.Positions = data.NewState
	encoded, err := state.encodeCursor(cursor)
	if err != nil {
		return PollResult{}, err
	}
	return PollResult{Cursor: encoded, Messages: messages}, nil
}

func (state *State) ResolveRecipient(username string) (*wechatdb.Recipient, error) {
	state.dbMu.Lock()
	defer state.dbMu.Unlock()
	material, err := state.readyMaterial()
	if err != nil {
		return nil, err
	}
	recipient, err := wechatdb.ResolveRecipient(material.DBDir, material.Keys, state.cacheDir(), username, material.WXID)
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
	material, err := state.readyMaterial()
	if err != nil {
		return nil, err
	}
	positions, err := wechatdb.RoomMessagePositionsFor(material.DBDir, material.Keys, state.cacheDir(), target)
	if err != nil {
		return nil, state.dbError("read WeChat message positions", err)
	}
	return positions, nil
}

func (state *State) HasTextMessageAfter(positions wechatdb.RoomMessagePositions, target, text string) (bool, error) {
	state.dbMu.Lock()
	defer state.dbMu.Unlock()
	material, err := state.readyMaterial()
	if err != nil {
		return false, err
	}
	found, err := wechatdb.HasOutgoingText(material.DBDir, material.Keys, state.cacheDir(), target, positions, text)
	if err != nil {
		return false, state.dbError("verify outgoing WeChat text", err)
	}
	return found, nil
}

func (state *State) readyMaterial() (keyFile, error) {
	if !state.IsInitialized() {
		return keyFile{}, errors.New("wechat automatic initialization is not complete")
	}
	material, err := state.readKey()
	if err != nil {
		state.RecordInitError(fmt.Errorf("load WeChat database keys: %w", err))
	}
	return material, err
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

func (state *State) cacheDir() string { return filepath.Join(state.stateDir, "cache") }

func (state *State) dbError(operation string, err error) error {
	wrapped := fmt.Errorf("%s: %w", operation, err)
	state.RecordInitError(wrapped)
	return wrapped
}

func (state *State) setError(message string) {
	state.errorMu.Lock()
	state.lastError = message
	state.errorMu.Unlock()
}

type orderKey struct {
	timestamp int64
	localID   int64
	room      string
}

func messageOrder(message map[string]any) orderKey {
	return orderKey{
		timestamp: integerField(message["msgtime"]),
		localID:   integerField(message["local_id"]),
		room:      stringField(message["roomid"]),
	}
}

func integerField(value any) int64 {
	switch value := value.(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	default:
		return 0
	}
}

func stringField(value any) string {
	valueString, _ := value.(string)
	return valueString
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

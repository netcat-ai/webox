package wechat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/netcat-ai/webox/internal/wechatdb"
	"github.com/netcat-ai/webox/wecom"
)

const (
	keyValidationPeriod             = 30 * time.Second
	mainWindowMissingGrace          = 15 * time.Second
	postLoginOverlayDismissAttempts = 3
	postLoginOverlayDismissInterval = time.Second
)

type InitializationState int

const (
	WaitingForLogin InitializationState = iota
	Ready
)

type State struct {
	stateDir string
	keyFile  string

	initialized         atomic.Bool
	lastValidationAt    atomic.Int64
	mainWindowMissingAt atomic.Int64
	dbMu                sync.Mutex
	database            *wechatdb.Store
	wxid                string
}

type keyFile struct {
	WXID  string            `json:"wxid"`
	DBDir string            `json:"dbDir"`
	Keys  map[string]string `json:"keys"`
}

type UserInfo struct {
	AccountID string
	WeChatID  string
	Nickname  string
	AvatarURL string
}

func New(stateDir string) *State {
	return &State{
		stateDir: stateDir,
		keyFile:  filepath.Join(stateDir, "wechat.key"),
	}
}

func (state *State) EnsureStateDir() error {
	return os.MkdirAll(state.stateDir, 0o700)
}

func (state *State) IsInitialized() bool {
	return state.initialized.Load()
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

func (state *State) InitializeIfReady() (InitializationState, error) {
	visible, known := wechatMainWindowReady()
	if !state.acceptMainWindowObservation(visible, known, time.Now()) {
		state.initialized.Store(false)
		return WaitingForLogin, nil
	}
	if !visible {
		return Ready, nil
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
		if err := state.database.ValidateSessionDB(); err == nil {
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
	if err := dismissPostLoginOverlay(); err != nil {
		_ = database.Close()
		return WaitingForLogin, err
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

func (state *State) acceptMainWindowObservation(visible, known bool, now time.Time) bool {
	if !known {
		state.mainWindowMissingAt.Store(0)
		return false
	}
	if visible {
		state.mainWindowMissingAt.Store(0)
		return true
	}
	if !state.IsInitialized() {
		return false
	}
	missingAt := state.mainWindowMissingAt.Load()
	if missingAt == 0 {
		state.mainWindowMissingAt.Store(now.UnixNano())
		return true
	}
	return now.Sub(time.Unix(0, missingAt)) < mainWindowMissingGrace
}

func (state *State) MarkUninitialized() {
	state.initialized.Store(false)
}

func dismissPostLoginOverlay() error {
	for attempt := range postLoginOverlayDismissAttempts {
		if attempt != 0 {
			time.Sleep(postLoginOverlayDismissInterval)
		}
		window := wechatMainWindow()
		if window == "" {
			return errors.New("wechat main window disappeared during post-login initialization")
		}
		if output, err := exec.Command("xdotool", "windowactivate", "--sync", window, "key", "--clearmodifiers", "Escape").CombinedOutput(); err != nil {
			return fmt.Errorf("run xdotool for post-login overlay: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func (state *State) RoomSessions() (map[string]int64, error) {
	state.dbMu.Lock()
	defer state.dbMu.Unlock()
	database, _, err := state.readyDatabase()
	if err != nil {
		return nil, err
	}
	sessions, err := database.EnabledRoomSessions()
	if err != nil {
		return nil, state.dbError("read enabled WeChat sessions", err)
	}
	return sessions, nil
}

func (state *State) RoomMessages(roomID string, after int64, limit int) ([]wecom.Message, error) {
	state.dbMu.Lock()
	defer state.dbMu.Unlock()
	database, wxid, err := state.readyDatabase()
	if err != nil {
		return nil, err
	}
	messages, err := database.MessagesForRoom(roomID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read WeChat Room messages: %w", err)
	}
	for index := range messages {
		if messages[index].Outgoing {
			messages[index].From = wxid
		}
	}
	return messages, nil
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

func (state *State) ContactsByRemark(remark string) ([]wechatdb.Contact, error) {
	state.dbMu.Lock()
	defer state.dbMu.Unlock()
	database, _, err := state.readyDatabase()
	if err != nil {
		return nil, err
	}
	contacts, err := database.ContactsByRemark(remark)
	if err != nil {
		return nil, state.dbError("query WeChat contacts", err)
	}
	return contacts, nil
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

func (state *State) ReadImage(roomID string, localID, createTime int64, md5 string) (*wechatdb.MediaFile, error) {
	state.dbMu.Lock()
	defer state.dbMu.Unlock()
	database, _, err := state.readyDatabase()
	if err != nil {
		return nil, err
	}
	media, err := database.ReadImage(roomID, localID, createTime, md5)
	if err != nil {
		return nil, fmt.Errorf("read WeChat image: %w", err)
	}
	return media, nil
}

func (state *State) ReadFile(roomID string, localID, createTime int64, filename string) (*wechatdb.LocalFile, error) {
	state.dbMu.Lock()
	defer state.dbMu.Unlock()
	database, _, err := state.readyDatabase()
	if err != nil {
		return nil, err
	}
	file, err := database.ReadFile(roomID, localID, createTime, filename)
	if err != nil {
		return nil, fmt.Errorf("read WeChat file: %w", err)
	}
	return file, nil
}

func (state *State) readyDatabase() (*wechatdb.Store, string, error) {
	if !state.IsInitialized() {
		return nil, "", errors.New("wechat automatic initialization is not complete")
	}
	if state.database == nil {
		state.MarkUninitialized()
		return nil, "", errors.New("database for WeChat is not open")
	}
	return state.database, state.wxid, nil
}

func openDatabase(material keyFile) (*wechatdb.Store, error) {
	database, err := wechatdb.Open(material.DBDir, material.Keys)
	if err != nil {
		return nil, err
	}
	if err := database.ValidateSessionDB(); err != nil {
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

func (state *State) dbError(operation string, err error) error {
	wrapped := fmt.Errorf("%s: %w", operation, err)
	state.MarkUninitialized()
	return wrapped
}

func wechatMainWindowReady() (bool, bool) {
	geometry, known := visibleWechatWindowGeometry()
	return mainWindowFromGeometry(geometry) != "", known
}

func wechatMainWindow() string {
	geometry, _ := visibleWechatWindowGeometry()
	return mainWindowFromGeometry(geometry)
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

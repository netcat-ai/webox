// Portions of this package are adapted from jackwener/wx-cli (Apache-2.0)
// and modified for Webox. See LICENSES/Apache-2.0.txt and THIRD_PARTY_NOTICES.md.
package wechatdb

import (
	"bytes"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "github.com/mutecomm/go-sqlcipher/v4"
	"github.com/netcat-ai/webox/wecom"
)

const (
	hexPatternLength = 96
	chunkSize        = 2 * 1024 * 1024
	pageSize         = 4096
	saltSize         = 16
)

type InitData struct {
	DBDir string
	WXID  string
	Keys  map[string]string
}

type MessagePosition struct {
	CreateTime int64 `json:"create_time"`
	LocalID    int64 `json:"local_id"`
}

type MessagePositions map[string]map[string]MessagePosition
type RoomMessagePositions map[string]MessagePosition

type OutgoingItem struct {
	Kind  string
	Value string
}

type storedMessage struct {
	localID   int64
	localType int64
	createdAt int64
	content   string
}

func messageByServerID(db *sql.DB, table string, serverID int64) (storedMessage, bool, error) {
	query := fmt.Sprintf(`SELECT local_id, local_type, create_time, message_content, WCDB_CT_message_content
        FROM [%s] WHERE server_id=? LIMIT 1`, table)
	var message storedMessage
	var content []byte
	var contentType sql.NullInt64
	err := db.QueryRow(query, serverID).Scan(&message.localID, &message.localType, &message.createdAt, &content, &contentType)
	if errors.Is(err, sql.ErrNoRows) {
		return storedMessage{}, false, nil
	}
	if err != nil {
		return storedMessage{}, false, err
	}
	message.content = decompressMessage(content, contentType.Int64)
	return message, true, nil
}

func messageResourceDB(store *Store, roomID string) (*sql.DB, int64, error) {
	db, found, err := store.database("message/message_resource.db")
	if err != nil || !found {
		return nil, 0, err
	}
	var chatID int64
	if err := db.QueryRow("SELECT rowid FROM ChatName2Id WHERE user_name=?", roomID).Scan(&chatID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	return db, chatID, nil
}

type PollData struct {
	Messages []wecom.Message
	NewState MessagePositions
	Skipped  []SkippedMessage
}

type SkippedMessage struct {
	MessageID    string
	Shard        string
	RealSenderID int64
}

type Recipient struct {
	Username   string
	SearchTerm string
}

type AccountInfo struct {
	AccountID string
	WeChatID  string
	Nickname  string
	AvatarURL string
}

type keyEntry struct {
	dbName string
	key    string
}

type Store struct {
	dbDir string
	keys  map[string]string
	mu    sync.Mutex
	dbs   map[string]*sql.DB
}

type messageShard struct {
	relativePath string
	db           *sql.DB
	table        string
}

type messageEvent struct {
	room         string
	shard        string
	db           *sql.DB
	position     MessagePosition
	realSenderID int64
	message      *wecom.Message
	skipped      *SkippedMessage
}

func DetectStorage() string {
	base := filepath.Join(wechatHome(), "xwechat_files")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	type candidate struct {
		path  string
		mtime time.Time
	}
	var candidates []candidate
	for _, entry := range entries {
		path := filepath.Join(base, entry.Name(), "db_storage")
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		candidates = append(candidates, candidate{path: path, mtime: latestDBMtime(path)})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].mtime.Before(candidates[j].mtime) })
	if len(candidates) == 0 {
		return ""
	}
	return candidates[len(candidates)-1].path
}

func AccountIDFromDBDir(dbDir string) string {
	raw := filepath.Base(filepath.Dir(dbDir))
	if strings.HasPrefix(raw, "wxid_") {
		parts := strings.Split(strings.TrimPrefix(raw, "wxid_"), "_")
		return "wxid_" + parts[0]
	}
	if index := strings.LastIndex(raw, "_"); index >= 0 {
		base, suffix := raw[:index], raw[index+1:]
		if len(suffix) == 4 && isHexString(suffix) {
			return base
		}
	}
	return strings.TrimSpace(raw)
}

func InitFromMemory() (InitData, error) {
	dbDir := DetectStorage()
	if dbDir == "" {
		return InitData{}, errors.New("未找到微信 db_storage 目录")
	}
	entries, err := scanKeys(dbDir)
	if err != nil {
		return InitData{}, err
	}
	keys := make(map[string]string, len(entries))
	for _, entry := range entries {
		keys[entry.dbName] = entry.key
	}
	if err := ValidateMessageDBKeys(dbDir, keys); err != nil {
		return InitData{}, err
	}
	wxid := AccountIDFromDBDir(dbDir)
	if wxid == "" {
		return InitData{}, errors.New("无法从微信数据库目录识别当前账号")
	}
	return InitData{DBDir: dbDir, WXID: wxid, Keys: keys}, nil
}

func Open(dbDir string, keys map[string]string) (*Store, error) {
	if strings.TrimSpace(dbDir) == "" {
		return nil, errors.New("WeChat database directory is empty")
	}
	if err := ValidateMessageDBKeys(dbDir, keys); err != nil {
		return nil, err
	}
	keyCopy := make(map[string]string, len(keys))
	for relative, key := range keys {
		keyCopy[relative] = key
	}
	return &Store{dbDir: dbDir, keys: keyCopy, dbs: make(map[string]*sql.DB)}, nil
}

func (store *Store) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	var result error
	for relative, db := range store.dbs {
		if err := db.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close %s: %w", relative, err))
		}
	}
	store.dbs = make(map[string]*sql.DB)
	return result
}

func (store *Store) CurrentSessionState() (map[string]int64, error) {
	return loadSessionState(store)
}

func (store *Store) BaselinePositions(startedAt int64) (MessagePositions, error) {
	sessions, err := loadSessionState(store)
	if err != nil {
		return nil, err
	}
	positions := make(MessagePositions)
	for username := range sessions {
		room := make(RoomMessagePositions)
		shards, err := findMessageShards(store, username)
		if err != nil {
			return nil, err
		}
		for _, shard := range shards {
			position, found, err := maxMessagePosition(shard.db, shard.table)
			if err != nil {
				continue
			}
			if !found {
				position = MessagePosition{CreateTime: startedAt}
			}
			room[shard.relativePath] = position
		}
		positions[username] = room
	}
	return positions, nil
}

func (store *Store) PollNewMessages(state MessagePositions, startedAt int64, limit int) (PollData, error) {
	return queryNewMessages(store, state, startedAt, limit)
}

func (store *Store) ResolveRecipient(raw, currentUserID string) (*Recipient, error) {
	username := strings.TrimSpace(raw)
	if username == "" {
		return nil, nil
	}
	db, found, err := store.database("contact/contact.db")
	if err != nil || !found {
		return nil, err
	}
	var storedUsername string
	var nickname, remark, alias sql.NullString
	err = db.QueryRow(
		"SELECT username, nick_name, remark, alias FROM contact WHERE delete_flag=0 AND username=? LIMIT 1",
		username,
	).Scan(&storedUsername, &nickname, &remark, &alias)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if storedUsername == currentUserID {
		return &Recipient{Username: storedUsername, SearchTerm: recipientDisplayName(storedUsername, nickname.String, remark.String, alias.String)}, nil
	}
	searchTerm := strings.TrimSpace(remark.String)
	if searchTerm == "" {
		return nil, errors.New("联系人或群聊必须设置唯一备注作为发送搜索词")
	}
	var duplicate string
	err = db.QueryRow(
		`SELECT username FROM contact
         WHERE delete_flag=0 AND username<>?
           AND (username=? OR nick_name=? OR remark=? OR alias=?)
         LIMIT 1`,
		storedUsername, searchTerm, searchTerm, searchTerm, searchTerm,
	).Scan(&duplicate)
	if err == nil {
		return nil, errors.New("联系人搜索词不唯一，请先设置唯一备注")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return &Recipient{Username: storedUsername, SearchTerm: searchTerm}, nil
}

func conversationRemarkFromDB(db *sql.DB, username string) (string, error) {
	var remark sql.NullString
	err := db.QueryRow(
		"SELECT remark FROM contact WHERE delete_flag=0 AND username=? LIMIT 1",
		strings.TrimSpace(username),
	).Scan(&remark)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(remark.String), nil
}

func (store *Store) ConversationRemark(username string) (string, error) {
	db, found, err := store.database("contact/contact.db")
	if err != nil || !found {
		return "", err
	}
	return conversationRemarkFromDB(db, username)
}

func (store *Store) AccountInfoFor(username string) (AccountInfo, error) {
	db, found, err := store.database("contact/contact.db")
	if err != nil {
		return AccountInfo{}, err
	}
	if !found {
		return AccountInfo{}, errors.New("contact database not found")
	}
	return accountInfoFromDB(db, username)
}

func accountInfoFromDB(db *sql.DB, username string) (AccountInfo, error) {
	username = strings.TrimSpace(username)
	var accountID string
	var alias, nickname, bigHeadURL, smallHeadURL sql.NullString
	err := db.QueryRow(
		`SELECT username, alias, nick_name, big_head_url, small_head_url
		 FROM contact WHERE delete_flag=0 AND username=? LIMIT 1`,
		username,
	).Scan(&accountID, &alias, &nickname, &bigHeadURL, &smallHeadURL)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountInfo{}, nil
	}
	if err != nil {
		return AccountInfo{}, err
	}
	wechatID := strings.TrimSpace(alias.String)
	if wechatID == "" {
		wechatID = strings.TrimSpace(accountID)
	}
	avatarURL := strings.TrimSpace(bigHeadURL.String)
	if avatarURL == "" {
		avatarURL = strings.TrimSpace(smallHeadURL.String)
	}
	return AccountInfo{
		AccountID: strings.TrimSpace(accountID),
		WeChatID:  wechatID,
		Nickname:  strings.TrimSpace(nickname.String),
		AvatarURL: avatarURL,
	}, nil
}

func (store *Store) RoomMessagePositionsFor(roomID string) (RoomMessagePositions, error) {
	shards, err := findMessageShards(store, roomID)
	if err != nil {
		return nil, err
	}
	positions := make(RoomMessagePositions)
	for _, shard := range shards {
		position, _, err := maxMessagePosition(shard.db, shard.table)
		if err != nil {
			continue
		}
		positions[shard.relativePath] = position
	}
	return positions, nil
}

func (store *Store) OutgoingItemsAfter(roomID string, positions RoomMessagePositions) ([]OutgoingItem, error) {
	shards, err := findMessageShards(store, roomID)
	if err != nil {
		return nil, err
	}
	group := strings.HasSuffix(roomID, "@chatroom")
	var items []OutgoingItem
	for _, shard := range shards {
		position := positions[shard.relativePath]
		db := shard.db
		query := fmt.Sprintf(`SELECT local_type, message_content, WCDB_CT_message_content FROM [%s]
            WHERE ((create_time > ?) OR (create_time = ? AND local_id > ?))
              AND status = 2 AND origin_source = 1
            ORDER BY create_time ASC, local_id ASC`, shard.table)
		rows, err := db.Query(query, position.CreateTime, position.CreateTime, position.LocalID)
		if err != nil {
			continue
		}
		for rows.Next() {
			var localType int64
			var content []byte
			var contentType sql.NullInt64
			if rows.Scan(&localType, &content, &contentType) != nil {
				continue
			}
			switch baseType(localType) {
			case 1:
				text := decompressMessage(content, contentType.Int64)
				items = append(items, OutgoingItem{Kind: "text", Value: stripGroupPrefix(text, group)})
			case 3:
				items = append(items, OutgoingItem{Kind: "image"})
			case 49:
				content := decompressMessage(content, contentType.Int64)
				if metadata, ok := parseFileMessage(content); ok {
					items = append(items, OutgoingItem{Kind: "file", Value: metadata.Filename})
				}
			}
		}
		rowsErr := rows.Err()
		_ = rows.Close()
		if rowsErr != nil {
			continue
		}
	}
	return items, nil
}

func scanKeys(dbDir string) ([]keyEntry, error) {
	pids := findWechatPIDs()
	if len(pids) == 0 {
		return nil, errors.New("找不到 WeChat 进程，请确认 WeChat 正在运行")
	}
	salts := collectDBSalts(dbDir)
	if len(salts) == 0 {
		return nil, errors.New("未找到加密数据库")
	}
	type rawKey struct{ key, salt string }
	var rawKeys []rawKey
	seen := make(map[rawKey]struct{})
	readableProcesses, scannedRegions, scannedBytes := 0, 0, 0
	for _, pid := range pids {
		regions, err := parseMaps(pid)
		if err != nil {
			continue
		}
		memory, err := os.Open(fmt.Sprintf("/proc/%d/mem", pid))
		if err != nil {
			continue
		}
		readableProcesses++
		scannedRegions += len(regions)
		for _, region := range regions {
			read, matches := scanRegion(memory, region[0], region[1])
			scannedBytes += read
			for _, match := range matches {
				candidate := rawKey{key: match[0], salt: match[1]}
				if _, exists := seen[candidate]; !exists {
					seen[candidate] = struct{}{}
					rawKeys = append(rawKeys, candidate)
				}
			}
		}
		_ = memory.Close()
	}
	var entries []keyEntry
	used := make(map[string]struct{})
	for _, raw := range rawKeys {
		for salt, dbName := range salts {
			if raw.salt != salt {
				continue
			}
			if _, exists := used[dbName]; exists {
				continue
			}
			used[dbName] = struct{}{}
			entries = append(entries, keyEntry{dbName: dbName, key: raw.key})
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf(
			"未从内存提取到有效 Message Key: readable_processes=%d, scanned_regions=%d, scanned_bytes=%d, key_candidates=%d, database_salts=%d",
			readableProcesses, scannedRegions, scannedBytes, len(rawKeys), len(salts),
		)
	}
	return entries, nil
}

func findWechatPIDs() []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		comm, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
		cmdline, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		name := strings.ToLower(strings.TrimSpace(string(comm)))
		command := strings.ToLower(strings.ReplaceAll(string(cmdline), "\x00", " "))
		if name == "wechat" || name == "weixin" || name == "wechatappex" ||
			strings.Contains(command, "/wechat/") || strings.Contains(command, "wechatappex") {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids
}

func parseMaps(pid int) ([][2]int64, error) {
	path := fmt.Sprintf("/proc/%d/maps", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	var regions [][2]int64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "rw") {
			continue
		}
		startRaw, endRaw, ok := strings.Cut(fields[0], "-")
		if !ok {
			continue
		}
		start, startErr := strconv.ParseInt(startRaw, 16, 64)
		end, endErr := strconv.ParseInt(endRaw, 16, 64)
		if startErr == nil && endErr == nil && end > start {
			regions = append(regions, [2]int64{start, end})
		}
	}
	return regions, nil
}

func scanRegion(memory *os.File, start, end int64) (int, [][2]string) {
	overlap := int64(hexPatternLength + 3)
	var scanned int
	var results [][2]string
	for offset := int64(0); offset < end-start; {
		length := min(int64(chunkSize), end-start-offset)
		buffer := make([]byte, int(length))
		read, err := memory.ReadAt(buffer, start+offset)
		if read > 0 {
			scanned += read
			results = append(results, searchKeyPatterns(buffer[:read])...)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			break
		}
		if length > overlap {
			offset += length - overlap
		} else {
			offset += length
		}
	}
	return scanned, results
}

func searchKeyPatterns(data []byte) [][2]string {
	total := hexPatternLength + 3
	var results [][2]string
	for index := 0; index+total <= len(data); {
		if data[index] != 'x' || data[index+1] != '\'' {
			index++
			continue
		}
		hexStart := index + 2
		candidate := data[hexStart : hexStart+hexPatternLength]
		if !isHexBytes(candidate) || data[hexStart+hexPatternLength] != '\'' {
			index++
			continue
		}
		results = append(results, [2]string{
			strings.ToLower(string(candidate[:64])),
			strings.ToLower(string(candidate[64:])),
		})
		index += total
	}
	return results
}

func collectDBSalts(dbDir string) map[string]string {
	result := make(map[string]string)
	_ = filepath.WalkDir(dbDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".db" {
			return nil
		}
		data := make([]byte, saltSize)
		file, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		_, readErr := io.ReadFull(file, data)
		_ = file.Close()
		if readErr != nil || bytes.Equal(data[:15], []byte("SQLite format 3")) {
			return nil
		}
		relative, relErr := filepath.Rel(dbDir, path)
		if relErr == nil {
			result[hex.EncodeToString(data)] = filepath.ToSlash(relative)
		}
		return nil
	})
	return result
}

func (store *Store) database(relative string) (*sql.DB, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if db := store.dbs[relative]; db != nil {
		return db, true, nil
	}
	keyHex, exists := store.keys[relative]
	if !exists {
		return nil, false, nil
	}
	dbPath := filepath.Join(store.dbDir, filepath.FromSlash(relative))
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		return nil, false, fmt.Errorf("密钥格式错误: %s", relative)
	}
	db, err := openSQLCipher(dbPath, hex.EncodeToString(key))
	if err != nil {
		return nil, false, fmt.Errorf("open encrypted WeChat database %s: %w", relative, err)
	}
	store.dbs[relative] = db
	return db, true, nil
}

func loadSessionState(store *Store) (map[string]int64, error) {
	db, found, err := store.database("session/session.db")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("无法打开 session.db")
	}
	rows, err := db.Query("SELECT username, last_timestamp FROM SessionTable WHERE last_timestamp > 0")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	sessions := make(map[string]int64)
	for rows.Next() {
		var username string
		var timestamp int64
		if rows.Scan(&username, &timestamp) == nil {
			sessions[username] = timestamp
		}
	}
	return sessions, rows.Err()
}

func ValidateMessageDBKeys(dbDir string, keys map[string]string) error {
	messageDir := filepath.Join(dbDir, "message")
	entries, err := os.ReadDir(messageDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read WeChat message database directory: %w", err)
	}
	var missing []string
	for _, entry := range entries {
		if entry.IsDir() || !isMessageShard(entry.Name()) {
			continue
		}
		relative := filepath.ToSlash(filepath.Join("message", entry.Name()))
		if strings.TrimSpace(keys[relative]) == "" {
			missing = append(missing, relative)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("missing WeChat message database key: %s", strings.Join(missing, ", "))
}

func messageDBKeys(store *Store) ([]string, error) {
	if err := ValidateMessageDBKeys(store.dbDir, store.keys); err != nil {
		return nil, err
	}
	var keys []string
	for key := range store.keys {
		if strings.HasPrefix(key, "message/") && isMessageShard(filepath.Base(key)) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func findMessageShards(store *Store, username string) ([]messageShard, error) {
	table := fmt.Sprintf("Msg_%x", md5.Sum([]byte(username)))
	var shards []messageShard
	keys, err := messageDBKeys(store)
	if err != nil {
		return nil, err
	}
	for _, relative := range keys {
		db, found, err := store.database(relative)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		var exists int
		err = db.QueryRow("SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		shards = append(shards, messageShard{relativePath: relative, db: db, table: table})
	}
	sort.Slice(shards, func(i, j int) bool { return shards[i].relativePath < shards[j].relativePath })
	return shards, nil
}

func queryNewMessages(store *Store, state MessagePositions, startedAt int64, limit int) (PollData, error) {
	sessions, err := loadSessionState(store)
	if err != nil {
		return PollData{}, err
	}
	var changed []string
	for username, timestamp := range sessions {
		lastKnown := startedAt
		for _, position := range state[username] {
			if position.CreateTime > lastKnown {
				lastKnown = position.CreateTime
			}
		}
		if timestamp >= lastKnown {
			changed = append(changed, username)
		}
	}
	sort.Strings(changed)
	if len(changed) == 0 {
		return PollData{Messages: []wecom.Message{}, NewState: state}, nil
	}
	perTableLimit := clamp(limit*4, 100, 2000)
	var events []messageEvent
	for _, username := range changed {
		shards, err := findMessageShards(store, username)
		if err != nil {
			return PollData{}, err
		}
		for _, shard := range shards {
			position, found := state[username][shard.relativePath]
			if !found {
				position = MessagePosition{CreateTime: startedAt}
			}
			rows, err := queryNewTable(shard, username, strings.HasSuffix(username, "@chatroom"), position, perTableLimit)
			if err != nil {
				continue
			}
			events = append(events, rows...)
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].position != events[j].position {
			if events[i].position.LocalID != events[j].position.LocalID {
				return events[i].position.LocalID < events[j].position.LocalID
			}
			return events[i].position.CreateTime < events[j].position.CreateTime
		}
		if events[i].room != events[j].room {
			return events[i].room < events[j].room
		}
		return events[i].shard < events[j].shard
	})
	events = events[:min(len(events), clamp(limit*10, 200, 5000))]
	if err := resolveEventSenders(events); err != nil {
		return PollData{}, err
	}
	return pollDataFromEvents(events, state, limit), nil
}

func resolveEventSenders(events []messageEvent) error {
	type shardLookup struct {
		db      *sql.DB
		ids     map[int64]struct{}
		indexes []int
	}
	byShard := make(map[string]*shardLookup)
	for index := range events {
		event := &events[index]
		lookup := byShard[event.shard]
		if lookup == nil {
			lookup = &shardLookup{db: event.db, ids: make(map[int64]struct{})}
			byShard[event.shard] = lookup
		}
		lookup.ids[event.realSenderID] = struct{}{}
		lookup.indexes = append(lookup.indexes, index)
	}
	for shard, lookup := range byShard {
		usernames, err := loadUsernamesByID(lookup.db, lookup.ids)
		if err != nil {
			return fmt.Errorf("resolve message senders from %s: %w", shard, err)
		}
		for _, index := range lookup.indexes {
			event := &events[index]
			if username := strings.TrimSpace(usernames[event.realSenderID]); username != "" {
				event.message.From = username
				continue
			}
			event.skipped = &SkippedMessage{
				MessageID:    event.message.MsgID,
				Shard:        event.shard,
				RealSenderID: event.realSenderID,
			}
			event.message = nil
		}
	}
	return nil
}

func loadUsernamesByID(db *sql.DB, ids map[int64]struct{}) (map[int64]string, error) {
	result := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	ordered := make([]int64, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	arguments := make([]any, len(ordered))
	placeholders := make([]string, len(ordered))
	for index, id := range ordered {
		arguments[index] = id
		placeholders[index] = "?"
	}
	rows, err := db.Query(
		"SELECT rowid, user_name FROM Name2Id WHERE rowid IN ("+strings.Join(placeholders, ",")+")",
		arguments...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var username sql.NullString
		if err := rows.Scan(&id, &username); err != nil {
			return nil, err
		}
		if username.Valid {
			result[id] = username.String
		}
	}
	return result, rows.Err()
}

func pollDataFromEvents(events []messageEvent, state MessagePositions, limit int) PollData {
	if state == nil {
		state = make(MessagePositions)
	}
	messages := make([]wecom.Message, 0, limit)
	var skipped []SkippedMessage
	for _, event := range events {
		if state[event.room] == nil {
			state[event.room] = make(RoomMessagePositions)
		}
		state[event.room][event.shard] = event.position
		if event.skipped != nil {
			skipped = append(skipped, *event.skipped)
			continue
		}
		if event.message != nil {
			messages = append(messages, *event.message)
			if len(messages) >= limit {
				break
			}
		}
	}
	return PollData{Messages: messages, NewState: state, Skipped: skipped}
}

func queryNewTable(shard messageShard, username string, group bool, position MessagePosition, limit int) ([]messageEvent, error) {
	db := shard.db
	query := fmt.Sprintf(`SELECT local_id, server_id, local_type, create_time, real_sender_id,
			message_content, WCDB_CT_message_content, status, origin_source
		 FROM [%s]`, shard.table)
	var arguments []any
	if position.LocalID > 0 {
		query += ` WHERE local_id > ? AND server_id > 0 ORDER BY local_id ASC LIMIT ?`
		arguments = []any{position.LocalID, limit}
	} else {
		query += ` WHERE (create_time > ? OR (create_time = ? AND local_id > ?))
		   AND server_id > 0
		 ORDER BY create_time ASC, local_id ASC LIMIT ?`
		arguments = []any{position.CreateTime, position.CreateTime, position.LocalID, limit}
	}
	rows, err := db.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var events []messageEvent
	for rows.Next() {
		var localID, serverID, localType, timestamp, realSenderID, status, originSource int64
		var contentType sql.NullInt64
		var content []byte
		if err := rows.Scan(&localID, &serverID, &localType, &timestamp, &realSenderID, &content, &contentType, &status, &originSource); err != nil {
			continue
		}
		decoded := decompressMessage(content, contentType.Int64)
		normalized := normalizeMessage(localType, decoded, group)
		messageID := strconv.FormatInt(serverID, 10)
		createdAt := saturatingMilliseconds(timestamp)
		message := wecom.Message{
			MsgID: messageID, Action: wecom.ActionSend,
			ToList: []string{}, RoomID: username, MsgTime: createdAt,
			Sequence: localID, Outgoing: originSource == 1,
		}
		switch normalized.kind {
		case "text":
			if normalized.referenceImageID != "" {
				message.MsgType = wecom.MessageTypeMixed
				message.Mixed = &wecom.Mixed{Items: []wecom.MixedItem{
					{Type: wecom.MessageTypeText, Content: normalized.text},
					{Type: wecom.MessageTypeImage, MessageID: normalized.referenceImageID},
				}}
			} else {
				message.MsgType = wecom.MessageTypeText
				message.Text = &wecom.Text{Content: normalized.text}
			}
		case "image":
			message.MsgType = wecom.MessageTypeImage
			message.Image = &wecom.Image{}
		case "voice":
			message.MsgType = wecom.MessageTypeVoice
			message.Voice = &wecom.Voice{}
		case "file":
			message.MsgType = wecom.MessageTypeFile
			message.File = &wecom.File{FileName: normalized.filename, FileExt: strings.TrimPrefix(filepath.Ext(normalized.filename), ".")}
		case "video":
			message.MsgType = wecom.MessageTypeVideo
			message.Video = &wecom.Video{}
		case "link":
			message.MsgType = wecom.MessageTypeLink
			message.Link = normalized.link
		case "sphfeed":
			message.MsgType = wecom.MessageTypeSphFeed
			message.SphFeed = normalized.sphFeed
		default:
			message.MsgType = normalized.kind
		}
		event := messageEvent{
			room: username, shard: shard.relativePath, db: shard.db,
			position:     MessagePosition{CreateTime: timestamp, LocalID: localID},
			realSenderID: realSenderID,
			message:      &message,
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func maxMessagePosition(db *sql.DB, table string) (MessagePosition, bool, error) {
	var position MessagePosition
	err := db.QueryRow(fmt.Sprintf(
		"SELECT create_time, local_id FROM [%s] ORDER BY local_id DESC LIMIT 1", table,
	)).Scan(&position.CreateTime, &position.LocalID)
	if errors.Is(err, sql.ErrNoRows) {
		return MessagePosition{}, false, nil
	}
	return position, err == nil, err
}

type normalizedContent struct {
	kind             string
	text             string
	filename         string
	referenceImageID string
	link             *wecom.Link
	sphFeed          *wecom.SphFeed
}

func normalizeMessage(localType int64, content string, group bool) normalizedContent {
	base := baseType(localType)
	if base == 1 {
		return normalizedContent{kind: "text", text: stripGroupPrefix(content, group)}
	}
	if base == 49 && strings.Contains(content, "<refermsg>") {
		document := stripGroupPrefix(content, group)
		message, ok := parseQuotedMessage(document)
		if !ok {
			return normalizedContent{kind: "text", text: quotedMessageText(document)}
		}
		reference := message.AppMessage.Reference
		messageID := cleanXMLText(reference.ServerID)
		if baseType(reference.Type) == 3 && messageID != "" {
			return normalizedContent{
				kind: "text", text: cleanXMLText(message.AppMessage.Title),
				referenceImageID: messageID,
			}
		}
		return normalizedContent{kind: "text", text: quotedMessageText(document)}
	}
	if base == 49 {
		if metadata, ok := parseFileMessage(content); ok {
			return normalizedContent{kind: "file", text: "[文件] " + metadata.Filename, filename: metadata.Filename}
		}
		if metadata, ok := parseLinkMessage(content); ok {
			if metadata.Kind == "finder" {
				return normalizedContent{kind: "sphfeed", sphFeed: metadata.SphFeed}
			}
			return normalizedContent{kind: "link", link: &wecom.Link{
				Title: metadata.Title, Description: metadata.Description, LinkURL: metadata.URL,
			}}
		}
	}
	labels := map[int64]struct{ kind, text string }{
		3: {"image", "[图片]"}, 34: {"voice", "[语音]"}, 42: {"card", "[名片]"},
		43: {"video", "[视频]"}, 47: {"emotion", "[表情]"}, 48: {"location", "[位置]"},
		49: {"link", "[链接]"}, 50: {"voip", "[通话]"}, 10000: {"system", "[系统消息]"},
		10002: {"revoke", "[撤回了一条消息]"},
	}
	if value, found := labels[base]; found {
		return normalizedContent{kind: value.kind, text: value.text}
	}
	return normalizedContent{kind: "unknown", text: stripGroupPrefix(content, group)}
}

type quotedMessage struct {
	AppMessage struct {
		Title     string          `xml:"title"`
		Reference quotedReference `xml:"refermsg"`
	} `xml:"appmsg"`
}

type quotedReference struct {
	Type     int64  `xml:"type"`
	ServerID string `xml:"svrid"`
	Content  string `xml:"content"`
}

type linkedMessage struct {
	AppMessage struct {
		Title       string `xml:"title"`
		Description string `xml:"des"`
		Type        int64  `xml:"type"`
		URL         string `xml:"url"`
		FinderFeed  struct {
			ObjectID    string `xml:"objectId"`
			FeedType    int64  `xml:"feedType"`
			Nickname    string `xml:"nickname"`
			Description string `xml:"desc"`
			MediaList   struct {
				Media []struct {
					URL          string `xml:"url"`
					FullCoverURL string `xml:"fullCoverUrl"`
					CoverURL     string `xml:"coverUrl"`
					ThumbURL     string `xml:"thumbUrl"`
					MediaType    int64  `xml:"mediaType"`
				} `xml:"media"`
			} `xml:"mediaList"`
		} `xml:"finderFeed"`
	} `xml:"appmsg"`
}

type linkMessageMetadata struct {
	Kind        string
	Title       string
	Description string
	URL         string
	SphFeed     *wecom.SphFeed
}

func parseLinkMessage(content string) (linkMessageMetadata, bool) {
	document := xmlMessageDocument(content)
	if document == "" {
		return linkMessageMetadata{}, false
	}
	var message linkedMessage
	if err := xml.Unmarshal([]byte(document), &message); err != nil {
		return linkMessageMetadata{}, false
	}
	if metadata, ok := finderFeedMetadata(message); ok {
		return metadata, true
	}
	metadata := linkMessageMetadata{
		Kind:        "link",
		Title:       cleanXMLText(message.AppMessage.Title),
		Description: cleanXMLText(message.AppMessage.Description),
		URL:         cleanXMLText(message.AppMessage.URL),
	}
	return metadata, metadata.Title != "" || metadata.Description != "" || metadata.URL != ""
}

func finderFeedMetadata(message linkedMessage) (linkMessageMetadata, bool) {
	feed := message.AppMessage.FinderFeed
	if message.AppMessage.Type != 51 && cleanXMLText(feed.ObjectID) == "" && cleanXMLText(feed.Nickname) == "" &&
		cleanXMLText(feed.Description) == "" && len(feed.MediaList.Media) == 0 {
		return linkMessageMetadata{}, false
	}
	sphFeed := &wecom.SphFeed{
		FeedType: message.AppMessage.FinderFeed.FeedType,
		SphName:  cleanXMLText(feed.Nickname),
		FeedDesc: cleanXMLText(feed.Description),
	}
	metadata := linkMessageMetadata{
		Kind:        "finder",
		Title:       sphFeed.SphName,
		Description: sphFeed.FeedDesc,
		SphFeed:     sphFeed,
	}
	for _, media := range feed.MediaList.Media {
		if metadata.URL == "" {
			for _, candidate := range []string{media.URL, media.FullCoverURL, media.CoverURL, media.ThumbURL} {
				if candidate = cleanXMLText(candidate); candidate != "" {
					metadata.URL = candidate
					break
				}
			}
		}
	}
	return metadata, metadata.Title != "" || metadata.Description != "" || metadata.URL != ""
}

func quotedMessageText(document string) string {
	message, ok := parseQuotedMessage(document)
	if !ok {
		return cleanXMLText(extractXMLText(document, "title"))
	}
	current := cleanXMLText(message.AppMessage.Title)
	reference := quotedReferenceText(message.AppMessage.Reference)
	if reference == "" {
		return current
	}
	if current == "" {
		return reference
	}
	return current + "\n" + reference
}

func parseQuotedMessage(document string) (quotedMessage, bool) {
	document = xmlMessageDocument(document)
	if document == "" {
		return quotedMessage{}, false
	}
	var message quotedMessage
	if err := xml.Unmarshal([]byte(document), &message); err != nil {
		return quotedMessage{}, false
	}
	return message, true
}

func quotedReferenceText(reference quotedReference) string {
	switch baseType(reference.Type) {
	case 1:
		content := cleanXMLText(reference.Content)
		if content == "" {
			return ""
		}
		return "[引用消息] " + content
	case 3:
		return "[引用消息][图片]"
	case 34:
		return "[引用消息][语音]"
	case 42:
		return "[引用消息][名片]"
	case 43:
		return "[引用消息][视频]"
	case 47:
		return "[引用消息][表情]"
	case 48:
		return "[引用消息][位置]"
	case 49:
		return quotedLinkText(reference.Content)
	case 50:
		return "[引用消息][通话]"
	case 10000:
		return "[引用消息][系统消息]"
	case 10002:
		return "[引用消息][撤回了一条消息]"
	default:
		return ""
	}
}

func compoundText(prefix string, values ...string) string {
	parts := make([]string, 0, len(values))
	seen := make(map[string]bool)
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			parts = append(parts, value)
			seen[value] = true
		}
	}
	if len(parts) == 0 {
		return prefix
	}
	return prefix + " " + strings.Join(parts, "\n")
}

func quotedLinkText(content string) string {
	metadata, ok := parseLinkMessage(content)
	if !ok {
		return "[引用消息][链接]"
	}
	if metadata.Kind == "finder" {
		return compoundText("[引用消息][视频号]", metadata.Title, metadata.Description, metadata.URL)
	}
	result := "[引用消息][链接]"
	if metadata.Title != "" {
		result += " " + metadata.Title
	}
	if metadata.URL != "" {
		result += "\n" + metadata.URL
	}
	return result
}

var (
	zstdOnce    sync.Once
	zstdDecoder *zstd.Decoder
)

func decompressMessage(data []byte, contentType int64) string {
	if contentType == 4 && len(data) > 0 {
		zstdOnce.Do(func() { zstdDecoder, _ = zstd.NewReader(nil) })
		if zstdDecoder != nil {
			if decoded, err := zstdDecoder.DecodeAll(data, nil); err == nil {
				return string(decoded)
			}
		}
	}
	return string(bytes.ToValidUTF8(data, []byte("�")))
}

func openSQLCipher(path, key string) (*sql.DB, error) {
	query := url.Values{
		"mode":                     {"ro"},
		"_busy_timeout":            {"5000"},
		"_query_only":              {"1"},
		"_pragma_cipher_page_size": {strconv.Itoa(pageSize)},
		"_pragma_key":              {fmt.Sprintf("x'%s'", key)},
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var schemaObjects int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&schemaObjects); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func latestDBMtime(root string) time.Time {
	var latest time.Time
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".db" {
			return nil
		}
		if info, infoErr := entry.Info(); infoErr == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
		return nil
	})
	return latest
}

func recipientDisplayName(username, nickname, remark, alias string) string {
	for _, value := range []string{remark, nickname, alias, username} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func wechatHome() string {
	for _, key := range []string{"WEBOX_WX_HOME", "WEBOX_HOME"} {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return "/webox/state/home"
}

func isMessageShard(name string) bool {
	if !strings.HasPrefix(name, "message_") || !strings.HasSuffix(name, ".db") ||
		strings.Contains(name, "_fts") || strings.Contains(name, "_resource") {
		return false
	}
	stem := strings.TrimSuffix(strings.TrimPrefix(name, "message_"), ".db")
	if stem == "" {
		return false
	}
	for _, character := range stem {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func baseType(value int64) int64 { return int64(uint64(value) & math.MaxUint32) }

func stripGroupPrefix(content string, group bool) string {
	if group {
		if _, value, found := strings.Cut(content, ":\n"); found {
			return value
		}
	}
	return content
}

func extractXMLText(document, tag string) string {
	open, close := "<"+tag+">", "</"+tag+">"
	start := strings.Index(document, open)
	if start < 0 {
		return ""
	}
	remainder := document[start+len(open):]
	end := strings.Index(remainder, close)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(remainder[:end])
}

func cleanXMLText(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(strings.TrimPrefix(value, "<![CDATA["), "]]>")
	replacer := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"", "&apos;", "'")
	return strings.TrimSpace(replacer.Replace(value))
}

func saturatingMilliseconds(seconds int64) int64 {
	if seconds > math.MaxInt64/1000 {
		return math.MaxInt64
	}
	if seconds < math.MinInt64/1000 {
		return math.MinInt64
	}
	return seconds * 1000
}

func isHexBytes(value []byte) bool {
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') && (character < 'A' || character > 'F') {
			return false
		}
	}
	return true
}

func isHexString(value string) bool { return isHexBytes([]byte(value)) }

func clamp(value, minimum, maximum int) int {
	return min(max(value, minimum), maximum)
}

// Portions of this package are adapted from jackwener/wx-cli (Apache-2.0)
// and modified for Webox. See LICENSES/Apache-2.0.txt and THIRD_PARTY_NOTICES.md.
package wechatdb

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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
	_ "github.com/mattn/go-sqlite3"
)

const (
	hexPatternLength = 96
	chunkSize        = 2 * 1024 * 1024
	pageSize         = 4096
	saltSize         = 16
	reserveSize      = 80
	walHeaderSize    = 32
	walFrameHeader   = 24
)

var sqliteHeader = []byte("SQLite format 3\x00")

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

type PollData struct {
	Messages []map[string]any
	NewState MessagePositions
}

type Recipient struct {
	Username   string
	SearchTerm string
}

type keyEntry struct {
	dbName string
	key    string
}

type mtimeEntry struct {
	DBMtime  int64  `json:"db_mt"`
	WALMtime int64  `json:"wal_mt"`
	Path     string `json:"path"`
}

type cacheEntry struct {
	dbMtime       int64
	walMtime      int64
	decryptedPath string
}

type dbCache struct {
	dbDir     string
	cacheDir  string
	mtimeFile string
	keys      map[string]string
	entries   map[string]cacheEntry
}

type messageShard struct {
	relativePath string
	path         string
	table        string
	maxTimestamp int64
}

type messageEvent struct {
	room     string
	shard    string
	position MessagePosition
	message  map[string]any
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
	wxid := AccountIDFromDBDir(dbDir)
	if wxid == "" {
		return InitData{}, errors.New("无法从微信数据库目录识别当前账号")
	}
	return InitData{DBDir: dbDir, WXID: wxid, Keys: keys}, nil
}

func CurrentSessionState(dbDir string, keys map[string]string, cacheDir string) (map[string]int64, error) {
	cache, err := newDBCache(dbDir, cacheDir, keys)
	if err != nil {
		return nil, err
	}
	return loadSessionState(cache)
}

func BaselinePositions(dbDir string, keys map[string]string, cacheDir string, startedAt int64) (MessagePositions, error) {
	cache, err := newDBCache(dbDir, cacheDir, keys)
	if err != nil {
		return nil, err
	}
	sessions, err := loadSessionState(cache)
	if err != nil {
		return nil, err
	}
	positions := make(MessagePositions)
	for username := range sessions {
		room := make(RoomMessagePositions)
		shards, err := findMessageShards(cache, username)
		if err != nil {
			return nil, err
		}
		for _, shard := range shards {
			position, found, err := maxMessagePosition(shard.path, shard.table)
			if err != nil {
				return nil, err
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

func PollNewMessages(dbDir string, keys map[string]string, state MessagePositions, startedAt int64, limit int, cacheDir string) (PollData, error) {
	cache, err := newDBCache(dbDir, cacheDir, keys)
	if err != nil {
		return PollData{}, err
	}
	return queryNewMessages(cache, state, startedAt, limit)
}

func ResolveRecipient(dbDir string, keys map[string]string, cacheDir, raw, currentUserID string) (*Recipient, error) {
	username := strings.TrimSpace(raw)
	if username == "" {
		return nil, nil
	}
	if username == "filehelper" {
		return &Recipient{Username: username, SearchTerm: "文件传输助手"}, nil
	}
	cache, err := newDBCache(dbDir, cacheDir, keys)
	if err != nil {
		return nil, err
	}
	path, found, err := cache.get("contact/contact.db")
	if err != nil || !found {
		return nil, err
	}
	db, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
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

func RoomMessagePositionsFor(dbDir string, keys map[string]string, cacheDir, roomID string) (RoomMessagePositions, error) {
	cache, err := newDBCache(dbDir, cacheDir, keys)
	if err != nil {
		return nil, err
	}
	shards, err := findMessageShards(cache, roomID)
	if err != nil {
		return nil, err
	}
	positions := make(RoomMessagePositions)
	for _, shard := range shards {
		position, _, err := maxMessagePosition(shard.path, shard.table)
		if err != nil {
			return nil, err
		}
		positions[shard.relativePath] = position
	}
	return positions, nil
}

func HasOutgoingText(dbDir string, keys map[string]string, cacheDir, roomID string, positions RoomMessagePositions, text string) (bool, error) {
	cache, err := newDBCache(dbDir, cacheDir, keys)
	if err != nil {
		return false, err
	}
	shards, err := findMessageShards(cache, roomID)
	if err != nil {
		return false, err
	}
	for _, shard := range shards {
		position := positions[shard.relativePath]
		db, err := openSQLite(shard.path)
		if err != nil {
			return false, err
		}
		query := fmt.Sprintf(`SELECT local_type, message_content, WCDB_CT_message_content
            FROM [%s]
            WHERE ((create_time > ?) OR (create_time = ? AND local_id > ?))
              AND status = 2 AND origin_source = 1
            ORDER BY create_time DESC, local_id DESC LIMIT 100`, shard.table)
		rows, err := db.Query(query, position.CreateTime, position.CreateTime, position.LocalID)
		if err != nil {
			_ = db.Close()
			return false, err
		}
		for rows.Next() {
			var localType int64
			var contentType sql.NullInt64
			var content []byte
			if err := rows.Scan(&localType, &content, &contentType); err != nil {
				continue
			}
			decoded := decompressMessage(content, contentType.Int64)
			if baseType(localType) == 1 && stripGroupPrefix(decoded, strings.HasSuffix(roomID, "@chatroom")) == text {
				_ = rows.Close()
				_ = db.Close()
				return true, nil
			}
		}
		_ = rows.Close()
		_ = db.Close()
	}
	return false, nil
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

func newDBCache(dbDir, cacheDir string, keys map[string]string) (*dbCache, error) {
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, err
	}
	cache := &dbCache{
		dbDir: dbDir, cacheDir: cacheDir, mtimeFile: filepath.Join(cacheDir, "_mtimes.json"),
		keys: keys, entries: make(map[string]cacheEntry),
	}
	cache.load()
	return cache, nil
}

func (cache *dbCache) load() {
	data, err := os.ReadFile(cache.mtimeFile)
	if err != nil {
		return
	}
	var stored map[string]mtimeEntry
	if json.Unmarshal(data, &stored) != nil {
		return
	}
	for relative, entry := range stored {
		if _, err := os.Stat(entry.Path); err == nil && fileMtime(cache.dbPath(relative)) == entry.DBMtime {
			cache.entries[relative] = cacheEntry{dbMtime: entry.DBMtime, walMtime: entry.WALMtime, decryptedPath: entry.Path}
		}
	}
}

func (cache *dbCache) save() {
	stored := make(map[string]mtimeEntry, len(cache.entries))
	for relative, entry := range cache.entries {
		stored[relative] = mtimeEntry{DBMtime: entry.dbMtime, WALMtime: entry.walMtime, Path: entry.decryptedPath}
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return
	}
	tmp := cache.mtimeFile + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, cache.mtimeFile)
	}
}

func (cache *dbCache) dbPath(relative string) string {
	return filepath.Join(cache.dbDir, filepath.FromSlash(relative))
}

func (cache *dbCache) get(relative string) (string, bool, error) {
	keyHex, exists := cache.keys[relative]
	if !exists {
		return "", false, nil
	}
	dbPath := cache.dbPath(relative)
	if _, err := os.Stat(dbPath); err != nil {
		return "", false, nil
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		return "", false, fmt.Errorf("密钥格式错误: %s", relative)
	}
	walPath := dbPath + "-wal"
	dbMtime, walMtime := fileMtime(dbPath), fileMtime(walPath)
	if entry, found := cache.entries[relative]; found && entry.dbMtime == dbMtime {
		if _, err := os.Stat(entry.decryptedPath); err == nil {
			if entry.walMtime != walMtime && walMtime != 0 {
				if err := applyWAL(walPath, entry.decryptedPath, key); err != nil {
					return "", false, err
				}
			}
			entry.walMtime = walMtime
			cache.entries[relative] = entry
			cache.save()
			return entry.decryptedPath, true, nil
		}
	}
	hash := md5.Sum([]byte(relative))
	outPath := filepath.Join(cache.cacheDir, fmt.Sprintf("%x.db", hash))
	if err := fullDecrypt(dbPath, outPath, key); err != nil {
		return "", false, err
	}
	if walMtime != 0 {
		if err := applyWAL(walPath, outPath, key); err != nil {
			return "", false, err
		}
	}
	cache.entries[relative] = cacheEntry{dbMtime: dbMtime, walMtime: walMtime, decryptedPath: outPath}
	cache.save()
	return outPath, true, nil
}

func loadSessionState(cache *dbCache) (map[string]int64, error) {
	path, found, err := cache.get("session/session.db")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("无法解密 session.db")
	}
	db, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
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

func messageDBKeys(cache *dbCache) []string {
	var keys []string
	for key := range cache.keys {
		if strings.HasPrefix(key, "message/") && isMessageShard(filepath.Base(key)) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func findMessageShards(cache *dbCache, username string) ([]messageShard, error) {
	table := fmt.Sprintf("Msg_%x", md5.Sum([]byte(username)))
	var shards []messageShard
	for _, relative := range messageDBKeys(cache) {
		path, found, err := cache.get(relative)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		db, err := openSQLite(path)
		if err != nil {
			return nil, err
		}
		var exists int
		err = db.QueryRow("SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			_ = db.Close()
			continue
		}
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		var maxTimestamp sql.NullInt64
		err = db.QueryRow(fmt.Sprintf("SELECT MAX(create_time) FROM [%s]", table)).Scan(&maxTimestamp)
		_ = db.Close()
		if err != nil {
			return nil, err
		}
		if maxTimestamp.Valid {
			shards = append(shards, messageShard{relativePath: relative, path: path, table: table, maxTimestamp: maxTimestamp.Int64})
		}
	}
	sort.Slice(shards, func(i, j int) bool { return shards[i].maxTimestamp > shards[j].maxTimestamp })
	return shards, nil
}

func queryNewMessages(cache *dbCache, state MessagePositions, startedAt int64, limit int) (PollData, error) {
	sessions, err := loadSessionState(cache)
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
		return PollData{Messages: []map[string]any{}, NewState: state}, nil
	}
	perTableLimit := clamp(limit*4, 100, 2000)
	var events []messageEvent
	for _, username := range changed {
		shards, err := findMessageShards(cache, username)
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
				return PollData{}, fmt.Errorf("query message table %s: %w", shard.table, err)
			}
			events = append(events, rows...)
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].position != events[j].position {
			if events[i].position.CreateTime != events[j].position.CreateTime {
				return events[i].position.CreateTime < events[j].position.CreateTime
			}
			return events[i].position.LocalID < events[j].position.LocalID
		}
		if events[i].room != events[j].room {
			return events[i].room < events[j].room
		}
		return events[i].shard < events[j].shard
	})
	if state == nil {
		state = make(MessagePositions)
	}
	messages := make([]map[string]any, 0, limit)
	for _, event := range events[:min(len(events), clamp(limit*10, 200, 5000))] {
		if state[event.room] == nil {
			state[event.room] = make(RoomMessagePositions)
		}
		state[event.room][event.shard] = event.position
		if event.message != nil {
			messages = append(messages, event.message)
			if len(messages) >= limit {
				break
			}
		}
	}
	return PollData{Messages: messages, NewState: state}, nil
}

func queryNewTable(shard messageShard, username string, group bool, position MessagePosition, limit int) ([]messageEvent, error) {
	db, err := openSQLite(shard.path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	idToUsername := loadIDToUsername(db)
	query := fmt.Sprintf(`SELECT local_id, server_id, local_type, create_time, real_sender_id,
            message_content, WCDB_CT_message_content, status, origin_source
         FROM [%s]
         WHERE create_time > ? OR (create_time = ? AND local_id > ?)
         ORDER BY create_time ASC, local_id ASC LIMIT ?`, shard.table)
	rows, err := db.Query(query, position.CreateTime, position.CreateTime, position.LocalID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var events []messageEvent
	for rows.Next() {
		var localID, serverID, localType, timestamp, realSenderID int64
		var contentType, status, originSource sql.NullInt64
		var content []byte
		if err := rows.Scan(&localID, &serverID, &localType, &timestamp, &realSenderID, &content, &contentType, &status, &originSource); err != nil {
			continue
		}
		event := messageEvent{
			room: username, shard: shard.relativePath,
			position: MessagePosition{CreateTime: timestamp, LocalID: localID},
		}
		if status.Int64 == 3 && originSource.Int64 == 2 {
			decoded := decompressMessage(content, contentType.Int64)
			messageType, text := normalizedMessage(localType, decoded, group)
			event.message = map[string]any{
				"msgid": strconv.FormatInt(serverID, 10), "local_id": localID,
				"action": "send", "from": senderUsername(realSenderID, decoded, group, username, idToUsername),
				"tolist": []any{}, "roomid": username, "msgtime": saturatingMilliseconds(timestamp),
				"msgtype": messageType, messageType: map[string]any{"content": text},
			}
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func loadIDToUsername(db *sql.DB) map[int64]string {
	result := make(map[int64]string)
	rows, err := db.Query("SELECT rowid, user_name FROM Name2Id")
	if err != nil {
		return result
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var username string
		if rows.Scan(&id, &username) == nil {
			result[id] = username
		}
	}
	return result
}

func maxMessagePosition(path, table string) (MessagePosition, bool, error) {
	db, err := openSQLite(path)
	if err != nil {
		return MessagePosition{}, false, err
	}
	defer func() { _ = db.Close() }()
	var position MessagePosition
	err = db.QueryRow(fmt.Sprintf(
		"SELECT create_time, local_id FROM [%s] ORDER BY create_time DESC, local_id DESC LIMIT 1", table,
	)).Scan(&position.CreateTime, &position.LocalID)
	if errors.Is(err, sql.ErrNoRows) {
		return MessagePosition{}, false, nil
	}
	return position, err == nil, err
}

func normalizedMessage(localType int64, content string, group bool) (string, string) {
	base := baseType(localType)
	if base == 1 {
		return "text", stripGroupPrefix(content, group)
	}
	if base == 49 && strings.Contains(content, "<refermsg>") {
		return "text", cleanXMLText(extractXMLText(stripGroupPrefix(content, group), "title"))
	}
	labels := map[int64]struct{ kind, text string }{
		3: {"image", "[图片]"}, 34: {"voice", "[语音]"}, 42: {"card", "[名片]"},
		43: {"video", "[视频]"}, 47: {"emotion", "[表情]"}, 48: {"location", "[位置]"},
		49: {"link", "[链接]"}, 50: {"voip", "[通话]"}, 10000: {"system", "[系统消息]"},
		10002: {"revoke", "[撤回了一条消息]"},
	}
	if value, found := labels[base]; found {
		return value.kind, value.text
	}
	return "unknown", stripGroupPrefix(content, group)
}

func senderUsername(realSenderID int64, content string, group bool, chatUsername string, idToUsername map[int64]string) string {
	sender := idToUsername[realSenderID]
	if group {
		if sender != "" && sender != chatUsername {
			return sender
		}
		if before, _, found := strings.Cut(content, ":\n"); found {
			return before
		}
		return ""
	}
	if sender != "" && sender != chatUsername {
		return sender
	}
	return chatUsername
}

func fullDecrypt(dbPath, outPath string, key []byte) error {
	input, err := os.Open(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return fmt.Errorf("数据库文件为空: %s", dbPath)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
		return err
	}
	output, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = output.Close() }()
	pages := (info.Size() + pageSize - 1) / pageSize
	buffer := make([]byte, pageSize)
	for page := int64(1); page <= pages; page++ {
		clear(buffer)
		remaining := info.Size() - (page-1)*pageSize
		expected := int64(pageSize)
		if remaining < expected {
			expected = remaining
		}
		if _, err := io.ReadFull(input, buffer[:expected]); err != nil {
			return err
		}
		decrypted, err := decryptPage(key, buffer, uint32(page))
		if err != nil {
			return err
		}
		if _, err := output.Write(decrypted); err != nil {
			return err
		}
	}
	return nil
}

func applyWAL(walPath, outPath string, key []byte) error {
	data, err := os.ReadFile(walPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || len(data) <= walHeaderSize {
		return err
	}
	salt1 := uint32FromBigEndian(data[16:20])
	salt2 := uint32FromBigEndian(data[20:24])
	frameSize := walFrameHeader + pageSize
	output, err := os.OpenFile(outPath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = output.Close() }()
	for position := walHeaderSize; position+frameSize <= len(data); position += frameSize {
		header := data[position : position+walFrameHeader]
		pageNumber := uint32FromBigEndian(header[:4])
		if pageNumber == 0 || pageNumber > 1_000_000 ||
			uint32FromBigEndian(header[8:12]) != salt1 || uint32FromBigEndian(header[12:16]) != salt2 {
			continue
		}
		page := data[position+walFrameHeader : position+frameSize]
		decrypted, err := decryptPage(key, page, pageNumber)
		if err != nil {
			return err
		}
		if _, err := output.WriteAt(decrypted, int64(pageNumber-1)*pageSize); err != nil {
			return err
		}
	}
	return nil
}

func decryptPage(key, page []byte, pageNumber uint32) ([]byte, error) {
	if len(page) < pageSize {
		return nil, fmt.Errorf("页面数据不足 %d 字节", pageSize)
	}
	ivOffset := pageSize - reserveSize
	iv := page[ivOffset : ivOffset+aes.BlockSize]
	result := make([]byte, pageSize)
	if pageNumber == 1 {
		decrypted, err := decryptCBC(key, iv, page[saltSize:pageSize-reserveSize])
		if err != nil {
			return nil, err
		}
		copy(result, sqliteHeader)
		copy(result[16:], decrypted)
		return result, nil
	}
	decrypted, err := decryptCBC(key, iv, page[:pageSize-reserveSize])
	if err != nil {
		return nil, err
	}
	copy(result, decrypted)
	return result, nil
}

func decryptCBC(key, iv, data []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("密文长度不是 AES 块大小的倍数: %d", len(data))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	result := append([]byte(nil), data...)
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(result, result)
	return result, nil
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

func openSQLite(path string) (*sql.DB, error) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_query_only=1"}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
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

func fileMtime(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
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

func uint32FromBigEndian(value []byte) uint32 {
	return uint32(value[0])<<24 | uint32(value[1])<<16 | uint32(value[2])<<8 | uint32(value[3])
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

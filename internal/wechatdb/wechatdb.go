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
	hexPatternLength    = 96
	chunkSize           = 2 * 1024 * 1024
	pageSize            = 4096
	saltSize            = 16
	fileAppMessageType  = 6
	enabledRoomCacheTTL = 30 * time.Second
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

type RoomMessagePositions map[string]MessagePosition

type OutgoingItem struct {
	Kind  string
	Value string
}

func messageResourceDB(store *Store, roomID string) (*sql.DB, int64, error) {
	db, found, err := store.database("message/message_resource.db")
	if err != nil {
		return nil, 0, err
	}
	if !found {
		return nil, 0, errors.New("message resource database not found")
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

type Recipient struct {
	Username   string
	SearchTerm string
}

type Contact struct {
	RoomID   string `json:"roomid"`
	Remark   string `json:"remark"`
	Nickname string `json:"nickname,omitempty"`
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
	dbDir               string
	keys                map[string]string
	mu                  sync.Mutex
	dbs                 map[string]*sql.DB
	sources             map[string]messageShard
	enabledRooms        []string
	enabledRoomsExpires time.Time
}

type messageShard struct {
	relativePath string
	db           *sql.DB
	table        string
}

type messageRecord struct {
	localID      int64
	serverID     int64
	localType    int64
	createdAt    int64
	realSenderID int64
	content      string
	originSource int64
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
	return raw
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
		return nil, errors.New("empty WeChat database directory")
	}
	if err := ValidateMessageDBKeys(dbDir, keys); err != nil {
		return nil, err
	}
	keyCopy := make(map[string]string, len(keys))
	for relative, key := range keys {
		keyCopy[relative] = key
	}
	return &Store{
		dbDir: dbDir, keys: keyCopy, dbs: make(map[string]*sql.DB),
		sources: make(map[string]messageShard),
	}, nil
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
	store.sources = make(map[string]messageShard)
	return result
}

func (store *Store) ValidateSessionDB() error {
	db, found, err := store.database("session/session.db")
	if err != nil {
		return err
	}
	if !found {
		return errors.New("session database not found")
	}
	var exists bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='SessionTable')").Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errors.New("session table not found")
	}
	return nil
}

func (store *Store) EnabledRoomSessions() (map[string]int64, error) {
	now := time.Now()
	if !now.Before(store.enabledRoomsExpires) {
		contactDB, found, err := store.database("contact/contact.db")
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New("contact database not found")
		}
		store.enabledRooms, err = loadEnabledRooms(contactDB)
		if err != nil {
			return nil, err
		}
		store.enabledRoomsExpires = now.Add(enabledRoomCacheTTL)
	}
	if len(store.enabledRooms) == 0 {
		return map[string]int64{}, nil
	}

	sessionDB, found, err := store.database("session/session.db")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("session database not found")
	}
	return loadRoomSessions(sessionDB, store.enabledRooms)
}

func (store *Store) MessagesForRoom(roomID string, after int64, limit int) ([]wecom.Message, error) {
	shard, found, err := store.messageSource(roomID)
	if err != nil || !found {
		return nil, err
	}
	records, err := queryMessageRecords(shard, after, limit)
	if err != nil {
		return nil, err
	}
	return convertMessages(shard, roomID, records)
}

func (store *Store) messageSource(roomID string) (messageShard, bool, error) {
	if source, ok := store.sources[roomID]; ok {
		return source, true, nil
	}
	shards, err := findMessageShards(store, roomID)
	if err != nil {
		return messageShard{}, false, err
	}
	if len(shards) == 0 {
		return messageShard{}, false, nil
	}
	if len(shards) > 1 {
		paths := make([]string, 0, len(shards))
		for _, shard := range shards {
			paths = append(paths, shard.relativePath)
		}
		return messageShard{}, false, fmt.Errorf("room %s message table exists in multiple databases: %s", roomID, strings.Join(paths, ", "))
	}
	store.sources[roomID] = shards[0]
	return shards[0], true, nil
}

func loadEnabledRooms(db *sql.DB) ([]string, error) {
	rows, err := db.Query("SELECT username FROM contact WHERE delete_flag=0 AND remark LIKE 'webox.%'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var username string
		if err := rows.Scan(&username); err != nil {
			return nil, err
		}
		names = append(names, username)
	}
	return names, rows.Err()
}

func loadRoomSessions(db *sql.DB, names []string) (map[string]int64, error) {
	arguments := make([]any, len(names))
	var placeholders strings.Builder
	for index, name := range names {
		arguments[index] = name
		placeholders.WriteString("?,")
	}
	rows, err := db.Query(
		"SELECT username, last_msg_locald_id FROM SessionTable WHERE last_msg_locald_id > 0 AND username IN ("+placeholders.String()[:placeholders.Len()-1]+")",
		arguments...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := make(map[string]int64, len(names))
	for rows.Next() {
		var username string
		var lastMessageLocalID int64
		if err := rows.Scan(&username, &lastMessageLocalID); err != nil {
			return nil, err
		}
		sessions[username] = lastMessageLocalID
	}
	return sessions, rows.Err()
}

func (store *Store) ResolveRecipient(raw, currentUserID string) (*Recipient, error) {
	username := raw
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
	searchTerm := remark.String
	if strings.TrimSpace(searchTerm) == "" {
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

func contactsByRemarkFromDB(db *sql.DB, remark string) ([]Contact, error) {
	rows, err := db.Query(
		`SELECT username, remark, nick_name FROM contact
		 WHERE delete_flag=0 AND remark=? ORDER BY username`,
		remark,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	contacts := make([]Contact, 0)
	for rows.Next() {
		var contact Contact
		var storedRemark, nickname sql.NullString
		if err := rows.Scan(&contact.RoomID, &storedRemark, &nickname); err != nil {
			return nil, err
		}
		contact.Remark = storedRemark.String
		contact.Nickname = strings.TrimSpace(nickname.String)
		contacts = append(contacts, contact)
	}
	return contacts, rows.Err()
}

func (store *Store) ContactsByRemark(remark string) ([]Contact, error) {
	db, found, err := store.database("contact/contact.db")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("contact database not found")
	}
	return contactsByRemarkFromDB(db, remark)
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
	wechatID := alias.String
	if wechatID == "" {
		wechatID = accountID
	}
	avatarURL := strings.TrimSpace(bigHeadURL.String)
	if avatarURL == "" {
		avatarURL = strings.TrimSpace(smallHeadURL.String)
	}
	return AccountInfo{
		AccountID: accountID,
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
				app, ok := parseAppMessage(content)
				filename := safeLocalFilename(cleanXMLText(app.AppMessage.Title))
				if ok && app.AppMessage.Type == fileAppMessageType && filename != "" {
					items = append(items, OutgoingItem{Kind: "file", Value: filename})
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

func queryMessageRecords(shard messageShard, after int64, limit int) ([]messageRecord, error) {
	query := fmt.Sprintf(`SELECT local_id, server_id, local_type, create_time, real_sender_id,
			message_content, WCDB_CT_message_content, origin_source
		 FROM [%s] WHERE local_id > ? AND server_id > 0
		 ORDER BY local_id ASC LIMIT ?`, shard.table)
	rows, err := shard.db.Query(query, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []messageRecord
	for rows.Next() {
		var record messageRecord
		var contentType sql.NullInt64
		var content []byte
		if err := rows.Scan(
			&record.localID, &record.serverID, &record.localType, &record.createdAt,
			&record.realSenderID, &content, &contentType, &record.originSource,
		); err != nil {
			return nil, err
		}
		record.content = decompressMessage(content, contentType.Int64)
		records = append(records, record)
	}
	return records, rows.Err()
}

func convertMessages(shard messageShard, roomID string, records []messageRecord) ([]wecom.Message, error) {
	ids := make(map[int64]struct{}, len(records))
	for _, record := range records {
		ids[record.realSenderID] = struct{}{}
	}
	usernames, err := loadUsernamesByID(shard.db, ids)
	if err != nil {
		return nil, fmt.Errorf("resolve message senders from %s: %w", shard.relativePath, err)
	}
	group := strings.HasSuffix(roomID, "@chatroom")
	messages := make([]wecom.Message, 0, len(records))
	for _, record := range records {
		from := usernames[record.realSenderID]
		if from == "" {
			from = strconv.FormatInt(record.realSenderID, 10)
		}
		message := wecom.Message{
			MsgID:    strconv.FormatInt(record.serverID, 10),
			Action:   wecom.ActionSend,
			From:     from,
			ToList:   []string{},
			RoomID:   roomID,
			MsgTime:  saturatingMilliseconds(record.createdAt),
			Sequence: record.localID,
			Outgoing: record.originSource == 1,
		}
		normalizeMessage(&message, record.localType, record.content, group)
		messages = append(messages, message)
	}
	return messages, nil
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

func normalizeMessage(message *wecom.Message, localType int64, content string, group bool) {
	switch baseType(localType) {
	case 1:
		message.MsgType = wecom.MessageTypeText
		message.Text = &wecom.Text{Content: stripGroupPrefix(content, group)}
	case 3:
		message.MsgType = wecom.MessageTypeImage
		message.Image = &wecom.Image{MD5Sum: strings.ToLower(xmlAttribute(content, "md5"))}
	case 34:
		message.MsgType = wecom.MessageTypeVoice
		message.Voice = &wecom.Voice{}
	case 42:
		message.MsgType = "card"
	case 43:
		message.MsgType = wecom.MessageTypeVideo
		message.Video = &wecom.Video{}
	case 47:
		message.MsgType = "emotion"
	case 48:
		message.MsgType = "location"
	case 49:
		normalizeAppMessage(message, content, group)
	case 50:
		message.MsgType = "voip"
	case 10000:
		message.MsgType = "system"
	case 10002:
		message.MsgType = "revoke"
	default:
		message.MsgType = "unknown"
	}
}

func normalizeAppMessage(message *wecom.Message, content string, group bool) {
	app, ok := parseAppMessage(content)
	if !ok {
		if strings.Contains(content, "<refermsg>") {
			message.MsgType = wecom.MessageTypeText
			message.Text = &wecom.Text{Content: cleanXMLText(extractXMLText(content, "title"))}
			return
		}
		message.MsgType = wecom.MessageTypeLink
		return
	}
	switch {
	case app.AppMessage.Reference != nil:
		message.MsgType = wecom.MessageTypeReply
		message.Reply = quotedReply(app, group)
	case app.AppMessage.Type == fileAppMessageType:
		filename := safeLocalFilename(cleanXMLText(app.AppMessage.Title))
		if filename == "" {
			message.MsgType = wecom.MessageTypeLink
			return
		}
		message.MsgType = wecom.MessageTypeFile
		message.File = &wecom.File{
			FileName: filename,
			FileExt:  strings.TrimPrefix(filepath.Ext(filename), "."),
		}
	default:
		if finder, found := finderFeedMetadata(app); found {
			message.MsgType = wecom.MessageTypeSphFeed
			message.SphFeed = finder.SphFeed
			return
		}
		metadata := linkMetadata(app)
		message.MsgType = wecom.MessageTypeLink
		message.Link = &wecom.Link{
			Title: metadata.Title, Description: metadata.Description, LinkURL: metadata.URL,
		}
	}
}

type quotedReference struct {
	Type       int64  `xml:"type"`
	ServerID   string `xml:"svrid"`
	FromUser   string `xml:"fromusr"`
	ChatUser   string `xml:"chatusr"`
	CreateTime int64  `xml:"createtime"`
	Content    string `xml:"content"`
}

func quotedReply(message appMessage, group bool) *wecom.Reply {
	reference := *message.AppMessage.Reference
	parent := wecom.Message{}
	normalizeMessage(&parent, reference.Type, reference.Content, false)
	from := cleanXMLText(reference.FromUser)
	if chatUser := cleanXMLText(reference.ChatUser); group && chatUser != "" {
		from = chatUser
	} else if from == "" {
		from = chatUser
	}
	return &wecom.Reply{
		Title: cleanXMLText(message.AppMessage.Title),
		Parent: wecom.MessageReference{
			MsgID:   cleanXMLText(reference.ServerID),
			From:    from,
			MsgType: parent.MsgType,
			MsgTime: saturatingMilliseconds(reference.CreateTime),
		},
	}
}

type appMessage struct {
	AppMessage struct {
		Title       string           `xml:"title"`
		Description string           `xml:"des"`
		Type        int64            `xml:"type"`
		URL         string           `xml:"url"`
		Reference   *quotedReference `xml:"refermsg"`
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
	Title       string
	Description string
	URL         string
	SphFeed     *wecom.SphFeed
}

func linkMetadata(message appMessage) linkMessageMetadata {
	return linkMessageMetadata{
		Title:       cleanXMLText(message.AppMessage.Title),
		Description: cleanXMLText(message.AppMessage.Description),
		URL:         cleanXMLText(message.AppMessage.URL),
	}
}

func finderFeedMetadata(message appMessage) (linkMessageMetadata, bool) {
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

func parseAppMessage(content string) (appMessage, bool) {
	document := xmlMessageDocument(content)
	if document == "" {
		return appMessage{}, false
	}
	var message appMessage
	if err := xml.Unmarshal([]byte(document), &message); err != nil {
		return appMessage{}, false
	}
	return message, true
}

func xmlMessageDocument(content string) string {
	for _, marker := range []string{"<?xml", "<msg"} {
		if index := strings.Index(content, marker); index >= 0 {
			return strings.TrimSpace(content[index:])
		}
	}
	return ""
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

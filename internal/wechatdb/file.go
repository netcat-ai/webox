package wechatdb

import (
	"database/sql"
	"encoding/binary"
	"encoding/xml"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const fileAppMessageType = 6

type LocalFile struct {
	Path     string
	Filename string
}

type fileAppMessage struct {
	AppMessage struct {
		Title string `xml:"title"`
		Type  int64  `xml:"type"`
	} `xml:"appmsg"`
}

type fileMessageMetadata struct {
	Filename string
}

func (store *Store) ReadFile(roomID, messageID string) (*LocalFile, error) {
	serverID, err := strconv.ParseInt(strings.TrimSpace(messageID), 10, 64)
	if err != nil || serverID <= 0 || strings.TrimSpace(roomID) == "" {
		return nil, nil
	}
	shards, err := findMessageShards(store, roomID)
	if err != nil {
		return nil, err
	}
	accountDir := filepath.Dir(store.dbDir)
	for _, shard := range shards {
		localID, createTime, metadata, found, err := fileMessage(shard.db, shard.table, serverID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		storedName, err := resourceFileName(store, roomID, localID, createTime)
		if err != nil {
			return nil, err
		}
		path := localFilePath(accountDir, createTime, storedName)
		if path == "" {
			path = localFilePath(accountDir, createTime, metadata.Filename)
		}
		if path == "" {
			return nil, nil
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, nil
		}
		filename := safeLocalFilename(metadata.Filename)
		if filename == "" {
			filename = filepath.Base(path)
		}
		return &LocalFile{Path: path, Filename: filename}, nil
	}
	return nil, nil
}

func fileMessage(db *sql.DB, table string, serverID int64) (int64, int64, fileMessageMetadata, bool, error) {
	message, found, err := messageByServerID(db, table, serverID)
	if err != nil || !found || baseType(message.localType) != 49 {
		return 0, 0, fileMessageMetadata{}, false, err
	}
	metadata, ok := parseFileMessage(message.content)
	return message.localID, message.createdAt, metadata, ok, nil
}

func parseFileMessage(content string) (fileMessageMetadata, bool) {
	document := xmlMessageDocument(content)
	if document == "" {
		return fileMessageMetadata{}, false
	}
	var message fileAppMessage
	if err := xml.Unmarshal([]byte(document), &message); err != nil || message.AppMessage.Type != fileAppMessageType {
		return fileMessageMetadata{}, false
	}
	filename := safeLocalFilename(cleanXMLText(message.AppMessage.Title))
	if filename == "" {
		return fileMessageMetadata{}, false
	}
	return fileMessageMetadata{Filename: filename}, true
}

func xmlMessageDocument(content string) string {
	for _, marker := range []string{"<?xml", "<msg"} {
		if index := strings.Index(content, marker); index >= 0 {
			return strings.TrimSpace(content[index:])
		}
	}
	return ""
}

func resourceFileName(store *Store, roomID string, localID, createTime int64) (string, error) {
	db, chatID, err := messageResourceDB(store, roomID)
	if err != nil {
		return "", err
	}
	if db == nil {
		return "", nil
	}
	rows, err := db.Query(`SELECT detail.packed_info
        FROM MessageResourceInfo AS info
        JOIN MessageResourceDetail AS detail ON detail.message_id=info.message_id
        WHERE info.chat_id=? AND info.message_local_id=?
          AND (info.message_local_type=49 OR info.message_local_type % 4294967296=49)
          AND info.message_create_time=?
        ORDER BY detail.status DESC, detail.resource_id DESC`, chatID, localID, createTime)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var packed []byte
		if rows.Scan(&packed) != nil {
			continue
		}
		_, stored := resourceDetailFileNames(packed)
		if stored = safeLocalFilename(stored); stored != "" {
			return stored, nil
		}
	}
	return "", rows.Err()
}

func resourceDetailFileNames(packed []byte) (string, string) {
	outer := protoBytesField(packed, 1)
	if len(outer) == 0 {
		return "", ""
	}
	return string(protoBytesField(outer, 1)), string(protoBytesField(outer, 2))
}

func protoBytesField(data []byte, fieldNumber uint64) []byte {
	for len(data) > 0 {
		tag, used := binary.Uvarint(data)
		if used <= 0 {
			return nil
		}
		data = data[used:]
		wireType := tag & 7
		field := tag >> 3
		switch wireType {
		case 0:
			_, used = binary.Uvarint(data)
			if used <= 0 {
				return nil
			}
			data = data[used:]
		case 1:
			if len(data) < 8 {
				return nil
			}
			data = data[8:]
		case 2:
			length, lengthBytes := binary.Uvarint(data)
			if lengthBytes <= 0 || length > uint64(len(data)-lengthBytes) {
				return nil
			}
			value := data[lengthBytes : lengthBytes+int(length)]
			if field == fieldNumber {
				return value
			}
			data = data[lengthBytes+int(length):]
		case 5:
			if len(data) < 4 {
				return nil
			}
			data = data[4:]
		default:
			return nil
		}
	}
	return nil
}

func localFilePath(accountDir string, createTime int64, filename string) string {
	filename = safeLocalFilename(filename)
	if filename == "" {
		return ""
	}
	root := filepath.Join(accountDir, "msg", "file")
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	path := filepath.Join(root, time.Unix(createTime, 0).Format("2006-01"), filename)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	relative, err := filepath.Rel(canonicalRoot, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ""
	}
	return resolved
}

func safeLocalFilename(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsRune(value, 0) || filepath.Base(value) != value || value == "." || value == ".." {
		return ""
	}
	return value
}

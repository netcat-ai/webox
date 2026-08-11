package wechatdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type LocalFile struct {
	Path     string
	Filename string
}

func (store *Store) ReadFile(roomID string, localID, createTime int64, filename string) (*LocalFile, error) {
	if roomID == "" || localID <= 0 || createTime <= 0 {
		return nil, errors.New("invalid WeChat file message location")
	}
	filename = safeLocalFilename(filename)
	if filename == "" {
		return nil, errors.New("invalid WeChat file name")
	}

	accountDir := filepath.Dir(store.dbDir)
	storedName, err := resourceFileName(store, roomID, localID, createTime)
	if err != nil {
		return nil, err
	}
	if storedName == "" {
		return nil, nil
	}
	path, err := localFilePath(accountDir, createTime, storedName)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("wechat file %s is not a regular file", path)
	}
	return &LocalFile{Path: path, Filename: filename}, nil
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
		if err := rows.Scan(&packed); err != nil {
			return "", err
		}
		stored, err := resourceStoredFilename(packed)
		if err != nil {
			return "", err
		}
		if stored = safeLocalFilename(stored); stored == "" {
			return "", errors.New("invalid stored WeChat file name")
		}
		return stored, nil
	}
	return "", rows.Err()
}

func resourceStoredFilename(packed []byte) (string, error) {
	outer, err := protoBytesField(packed, 1)
	if err != nil {
		return "", fmt.Errorf("decode WeChat file resource: %w", err)
	}
	stored, err := protoBytesField(outer, 2)
	if err != nil {
		return "", fmt.Errorf("decode stored WeChat file name: %w", err)
	}
	return string(stored), nil
}

func protoBytesField(data []byte, fieldNumber uint64) ([]byte, error) {
	for len(data) > 0 {
		tag, used := binary.Uvarint(data)
		if used <= 0 {
			return nil, errors.New("invalid protobuf tag")
		}
		data = data[used:]
		wireType := tag & 7
		field := tag >> 3
		switch wireType {
		case 0:
			_, used = binary.Uvarint(data)
			if used <= 0 {
				return nil, errors.New("invalid protobuf varint")
			}
			data = data[used:]
		case 1:
			if len(data) < 8 {
				return nil, errors.New("truncated protobuf fixed64 field")
			}
			data = data[8:]
		case 2:
			length, lengthBytes := binary.Uvarint(data)
			if lengthBytes <= 0 || length > uint64(len(data)-lengthBytes) {
				return nil, errors.New("invalid protobuf bytes field")
			}
			value := data[lengthBytes : lengthBytes+int(length)]
			if field == fieldNumber {
				return value, nil
			}
			data = data[lengthBytes+int(length):]
		case 5:
			if len(data) < 4 {
				return nil, errors.New("truncated protobuf fixed32 field")
			}
			data = data[4:]
		default:
			return nil, fmt.Errorf("unsupported protobuf wire type %d", wireType)
		}
	}
	return nil, fmt.Errorf("protobuf field %d not found", fieldNumber)
}

func localFilePath(accountDir string, createTime int64, filename string) (string, error) {
	filename = safeLocalFilename(filename)
	if filename == "" {
		return "", errors.New("invalid WeChat file name")
	}
	root := filepath.Join(accountDir, "msg", "file")
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, time.Unix(createTime, 0).Format("2006-01"), filename)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(canonicalRoot, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("wechat file path escapes the file directory")
	}
	return resolved, nil
}

func safeLocalFilename(value string) string {
	if value == "" || strings.ContainsRune(value, 0) || filepath.Base(value) != value || value == "." || value == ".." {
		return ""
	}
	return value
}

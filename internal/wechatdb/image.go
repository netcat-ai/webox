package wechatdb

import (
	"bytes"
	"crypto/aes"
	"crypto/md5"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	v2ImageMagic = []byte{0x07, 0x08, 'V', '2', 0x08, 0x07}
	imageKeys    sync.Map
)

const (
	v2ImageHeaderSize  = 15
	imageThumbnailWait = 3 * time.Second
)

type MediaFile struct {
	Data        []byte
	ContentType string
	Filename    string
}

type imageKeyMaterial struct {
	aes [16]byte
	xor byte
}

type v2ImageCandidate struct {
	path    string
	modTime int64
}

func (store *Store) ReadImage(roomID, messageID string) (*MediaFile, error) {
	serverID, err := strconv.ParseInt(strings.TrimSpace(messageID), 10, 64)
	if err != nil || serverID <= 0 || strings.TrimSpace(roomID) == "" {
		return nil, nil
	}
	shards, err := findMessageShards(store, roomID)
	if err != nil {
		return nil, err
	}
	accountDir := filepath.Dir(store.dbDir)
	attachRoot := filepath.Join(accountDir, "msg", "attach")
	for _, shard := range shards {
		localID, createTime, content, found, err := imageMessage(shard.db, shard.table, serverID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		md5Value, err := resourceImageMD5(store, roomID, localID, createTime)
		if err != nil {
			return nil, err
		}
		if md5Value == "" {
			md5Value = xmlAttribute(content, "md5")
		}
		if !isHexValue(md5Value, 32) {
			continue
		}
		var decodeErr error
		for _, path := range findImageDats(attachRoot, roomID, strings.ToLower(md5Value)) {
			media, err := decodeImageDat(accountDir, attachRoot, path, strings.ToLower(md5Value))
			if err == nil {
				return media, nil
			}
			decodeErr = errors.Join(decodeErr, fmt.Errorf("decode %s: %w", filepath.Base(path), err))
		}
		if decodeErr != nil {
			return nil, decodeErr
		}
	}
	return nil, nil
}

func imageMessage(db *sql.DB, table string, serverID int64) (int64, int64, string, bool, error) {
	message, found, err := messageByServerID(db, table, serverID)
	if err != nil || !found || baseType(message.localType) != 3 {
		return 0, 0, "", false, err
	}
	return message.localID, message.createdAt, message.content, true, nil
}

func resourceImageMD5(store *Store, roomID string, localID, createTime int64) (string, error) {
	db, chatID, err := messageResourceDB(store, roomID)
	if err != nil {
		return "", err
	}
	if db == nil {
		return "", nil
	}
	var packed []byte
	err = db.QueryRow(`SELECT packed_info FROM MessageResourceInfo
        WHERE chat_id=? AND message_local_id=?
          AND (message_local_type=3 OR message_local_type % 4294967296=3)
          AND message_create_time=? ORDER BY rowid DESC LIMIT 1`, chatID, localID, createTime).Scan(&packed)
	if errors.Is(err, sql.ErrNoRows) {
		err = db.QueryRow(`SELECT packed_info FROM MessageResourceInfo
            WHERE chat_id=? AND message_local_id=?
              AND (message_local_type=3 OR message_local_type % 4294967296=3)
            ORDER BY message_create_time DESC LIMIT 1`, chatID, localID).Scan(&packed)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return resourceMD5(packed), nil
}

func resourceMD5(packed []byte) string {
	marker := []byte{0x12, 0x22, 0x0a, 0x20}
	if index := bytes.Index(packed, marker); index >= 0 {
		start := index + len(marker)
		if start+32 <= len(packed) && isHexValue(string(packed[start:start+32]), 32) {
			return strings.ToLower(string(packed[start : start+32]))
		}
	}
	for index := 0; index+32 <= len(packed); index++ {
		value := string(packed[index : index+32])
		if isHexValue(value, 32) {
			return strings.ToLower(value)
		}
	}
	return ""
}

func findImageDats(attachRoot, roomID, md5Value string) []string {
	return findImageDatsAt(attachRoot, roomID, md5Value, time.Now())
}

func findImageDatsAt(attachRoot, roomID, md5Value string, now time.Time) []string {
	chatDir := filepath.Join(attachRoot, fmt.Sprintf("%x", md5.Sum([]byte(roomID))))
	entries, err := os.ReadDir(chatDir)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	for _, month := range entries {
		if !month.IsDir() {
			continue
		}
		var paths []string
		for _, suffix := range []string{"_h.dat", ".dat", "_t.dat"} {
			path := filepath.Join(chatDir, month.Name(), "Img", md5Value+suffix)
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				if suffix == "_t.dat" && now.Before(info.ModTime().Add(imageThumbnailWait)) {
					continue
				}
				paths = append(paths, path)
			}
		}
		if len(paths) != 0 {
			return paths
		}
	}
	return nil
}

func decodeImageDat(accountDir, attachRoot, path, md5Value string) (*MediaFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() <= 0 {
		return nil, errors.New("local WeChat image has an invalid size")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(data, v2ImageMagic) {
		return nil, errors.New("unsupported local WeChat image: only V2 .dat is supported")
	}
	key, err := imageKey(accountDir, attachRoot)
	if err != nil {
		return nil, err
	}
	data, err = decodeV2Image(data, key)
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(data, []byte("wxgf")) {
		data, err = convertWXGFToJPEG(data)
		if err != nil {
			return nil, err
		}
	}
	contentType := imageContentType(data)
	if contentType == "" {
		return nil, errors.New("decoded WeChat image has an unsupported format")
	}
	return &MediaFile{Data: data, ContentType: contentType, Filename: md5Value + imageExtension(contentType)}, nil
}

func convertWXGFToJPEG(data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, []byte("wxgf")) {
		return nil, errors.New("invalid wxgf image")
	}
	jpeg, err := decodeWXGF(data)
	if err != nil {
		return nil, err
	}
	if imageContentType(jpeg) != "image/jpeg" {
		return nil, errors.New("WeChat WXGF decoder did not produce a JPEG image")
	}
	return jpeg, nil
}

func imageKey(accountDir, attachRoot string) (imageKeyMaterial, error) {
	if value, found := imageKeys.Load(accountDir); found {
		return value.(imageKeyMaterial), nil
	}
	files, err := v2ImageFiles(attachRoot)
	if err != nil {
		return imageKeyMaterial{}, err
	}
	key, err := deriveImageKey(accountDir, files)
	if err == nil {
		imageKeys.Store(accountDir, key)
	}
	return key, err
}

func v2ImageFiles(root string) ([]v2ImageCandidate, error) {
	var result []v2ImageCandidate
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".dat") {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		header := make([]byte, len(v2ImageMagic))
		_, readErr := file.Read(header)
		_ = file.Close()
		if readErr != nil || !bytes.Equal(header, v2ImageMagic) {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			result = append(result, v2ImageCandidate{path: path, modTime: info.ModTime().UnixNano()})
		}
		return nil
	})
	sort.Slice(result, func(i, j int) bool { return result[i].modTime > result[j].modTime })
	return result, err
}

func deriveImageKey(accountDir string, files []v2ImageCandidate) (imageKeyMaterial, error) {
	if len(files) == 0 {
		return imageKeyMaterial{}, errors.New("no V2 WeChat image samples are available")
	}
	xorKey, err := imageXORKey(files)
	if err != nil {
		return imageKeyMaterial{}, err
	}
	templates, err := imageTemplateBlocks(files, 3)
	if err != nil {
		return imageKeyMaterial{}, err
	}
	raw := filepath.Base(accountDir)
	separator := strings.LastIndexByte(raw, '_')
	if separator < 1 || len(raw)-separator-1 != 4 {
		return imageKeyMaterial{}, errors.New("WeChat account directory has no four-hex suffix")
	}
	suffix, err := hex.DecodeString(raw[separator+1:])
	if err != nil || len(suffix) != 2 {
		return imageKeyMaterial{}, errors.New("WeChat account directory has an invalid suffix")
	}
	for _, wxid := range []string{raw[:separator], raw} {
		if aesKey, found := bruteForceImageKey(xorKey, suffix, wxid, templates); found {
			return imageKeyMaterial{aes: aesKey, xor: xorKey}, nil
		}
	}
	return imageKeyMaterial{}, errors.New("could not derive the local WeChat image key")
}

func imageXORKey(files []v2ImageCandidate) (byte, error) {
	counts := make(map[byte]int)
	checked := 0
	for _, file := range files {
		if !strings.HasSuffix(file.path, "_t.dat") {
			continue
		}
		data, err := os.ReadFile(file.path)
		if err != nil || len(data) < v2ImageHeaderSize+18 {
			continue
		}
		left, right := data[len(data)-2]^0xff, data[len(data)-1]^0xd9
		if left == right {
			counts[left]++
		}
		checked++
		if checked >= 32 {
			break
		}
	}
	var key byte
	best := 0
	for candidate, count := range counts {
		if count > best {
			key, best = candidate, count
		}
	}
	if best < 1 {
		return 0, errors.New("could not derive the local WeChat image XOR key")
	}
	return key, nil
}

func imageTemplateBlocks(files []v2ImageCandidate, limit int) ([][16]byte, error) {
	seen := make(map[[16]byte]struct{})
	var result [][16]byte
	for pass := 0; pass < 2 && len(result) == 0; pass++ {
		for _, file := range files {
			if pass == 0 && !strings.HasSuffix(file.path, "_t.dat") {
				continue
			}
			data, err := os.ReadFile(file.path)
			if err != nil || len(data) < v2ImageHeaderSize+aes.BlockSize {
				continue
			}
			var block [16]byte
			copy(block[:], data[v2ImageHeaderSize:v2ImageHeaderSize+aes.BlockSize])
			if _, found := seen[block]; found {
				continue
			}
			seen[block] = struct{}{}
			result = append(result, block)
			if len(result) >= limit {
				return result, nil
			}
		}
	}
	if len(result) == 0 {
		return nil, errors.New("no V2 WeChat image ciphertext templates are available")
	}
	return result, nil
}

func bruteForceImageKey(xorKey byte, suffix []byte, wxid string, templates [][16]byte) ([16]byte, bool) {
	workers := max(runtime.NumCPU(), 1)
	var stop atomic.Bool
	result := make(chan [16]byte, 1)
	var group sync.WaitGroup
	total := uint32(1 << 24)
	chunk := total / uint32(workers)
	for worker := 0; worker < workers; worker++ {
		start := uint32(worker) * chunk
		end := start + chunk
		if worker == workers-1 {
			end = total
		}
		group.Add(1)
		go func() {
			defer group.Done()
			for upper := start; upper < end && !stop.Load(); upper++ {
				uin := (upper << 8) | uint32(xorKey)
				uinText := strconv.FormatUint(uint64(uin), 10)
				digest := md5.Sum([]byte(uinText))
				if digest[0] != suffix[0] || digest[1] != suffix[1] {
					continue
				}
				aesDigest := md5.Sum([]byte(uinText + wxid))
				aesHex := hex.EncodeToString(aesDigest[:])
				var key [16]byte
				copy(key[:], aesHex[:16])
				if verifyImageKey(key, templates) && stop.CompareAndSwap(false, true) {
					result <- key
				}
			}
		}()
	}
	group.Wait()
	close(result)
	key, found := <-result
	return key, found
}

func verifyImageKey(key [16]byte, templates [][16]byte) bool {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return false
	}
	for _, template := range templates {
		var plain [16]byte
		block.Decrypt(plain[:], template[:])
		if imageContentType(plain[:]) == "" && !bytes.HasPrefix(plain[:], []byte("wxgf")) {
			return false
		}
	}
	return len(templates) != 0
}

func decodeV2Image(data []byte, key imageKeyMaterial) ([]byte, error) {
	if len(data) < v2ImageHeaderSize || !bytes.HasPrefix(data, v2ImageMagic) {
		return nil, errors.New("invalid V2 WeChat image header")
	}
	aesSize := int(binary.LittleEndian.Uint32(data[6:10]))
	xorSize := int(binary.LittleEndian.Uint32(data[10:14]))
	alignedAESSize := aesSize + (aes.BlockSize - aesSize%aes.BlockSize)
	aesEnd, rawEnd := v2ImageHeaderSize+alignedAESSize, len(data)-xorSize
	if aesEnd > rawEnd || rawEnd > len(data) {
		return nil, errors.New("invalid V2 WeChat image segment lengths")
	}
	block, err := aes.NewCipher(key.aes[:])
	if err != nil {
		return nil, err
	}
	plainAES := make([]byte, alignedAESSize)
	for offset := 0; offset < alignedAESSize; offset += aes.BlockSize {
		block.Decrypt(plainAES[offset:offset+aes.BlockSize], data[v2ImageHeaderSize+offset:v2ImageHeaderSize+offset+aes.BlockSize])
	}
	plainAES, err = unpadImage(plainAES)
	if err != nil {
		return nil, err
	}
	result := append([]byte{}, plainAES...)
	result = append(result, data[aesEnd:rawEnd]...)
	for _, value := range data[rawEnd:] {
		result = append(result, value^key.xor)
	}
	return result, nil
}

func unpadImage(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("empty image AES plaintext")
	}
	padding := int(data[len(data)-1])
	if padding < 1 || padding > aes.BlockSize || padding > len(data) {
		return nil, errors.New("invalid image PKCS7 padding")
	}
	for _, value := range data[len(data)-padding:] {
		if int(value) != padding {
			return nil, errors.New("invalid image PKCS7 padding bytes")
		}
	}
	return data[:len(data)-padding], nil
}

func imageContentType(data []byte) string {
	switch {
	case len(data) >= 3 && bytes.Equal(data[:3], []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return "image/png"
	case len(data) >= 6 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))):
		return "image/gif"
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	default:
		return ""
	}
}

func imageExtension(contentType string) string {
	return map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp"}[contentType]
}

func isHexValue(value string, length int) bool {
	if len(value) != length {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func xmlAttribute(document, name string) string {
	for _, quote := range []string{"\"", "'"} {
		prefix := name + "=" + quote
		start := strings.Index(document, prefix)
		if start < 0 {
			continue
		}
		start += len(prefix)
		if end := strings.Index(document[start:], quote); end >= 0 {
			return document[start : start+end]
		}
	}
	return ""
}

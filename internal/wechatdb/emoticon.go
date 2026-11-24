package wechatdb

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/netcat-ai/webox/wecom"
)

const maxEmoticonBytes = 10 << 20

func setEmoticon(message *wecom.Message, hashes ...string) {
	for _, hash := range hashes {
		hash = strings.ToLower(strings.TrimSpace(hash))
		if isHexValue(hash, 32) {
			message.MsgType = wecom.MessageTypeImage
			message.Image = &wecom.Image{MD5Sum: hash, Emoticon: true}
			return
		}
	}
}

func (store *Store) ReadEmoticon(createTime int64, hash string) (*MediaFile, error) {
	if createTime <= 0 || !isHexValue(hash, 32) {
		return nil, nil
	}
	hash = strings.ToLower(hash)
	accountDir := filepath.Dir(store.dbDir)
	account, err := os.OpenRoot(accountDir)
	if err != nil {
		return nil, err
	}
	defer account.Close()

	month := time.Unix(createTime, 0).Format("2006-01")
	cache := filepath.Join("cache", month, "Emoticon", hash[:2], hash)
	file, err := account.OpenFile(cache, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxEmoticonBytes+aes.BlockSize {
		return nil, ErrImageDecode
	}
	data, err := io.ReadAll(io.LimitReader(file, maxEmoticonBytes+aes.BlockSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > maxEmoticonBytes+aes.BlockSize {
		return nil, ErrImageDecode
	}
	if imageContentType(data) == "" {
		keys, err := imageKey(accountDir, filepath.Join(accountDir, "msg", "attach"))
		if err != nil || len(data)%aes.BlockSize != 0 {
			return nil, ErrImageDecode
		}
		block, err := aes.NewCipher(keys.emoticon[:])
		if err != nil {
			return nil, ErrImageDecode
		}
		cipher.NewCBCDecrypter(block, keys.emoticon[:]).CryptBlocks(data, data)
		data, err = unpadImage(data)
		if err != nil {
			return nil, ErrImageDecode
		}
	}
	contentType := imageContentType(data)
	if contentType == "" || len(data) > maxEmoticonBytes {
		return nil, ErrImageDecode
	}
	return &MediaFile{Data: data, ContentType: contentType, Filename: hash + imageExtension(contentType)}, nil
}

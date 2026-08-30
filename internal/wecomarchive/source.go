package wecomarchive

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/netcat-ai/webox/internal/sharedmedia"
	"github.com/netcat-ai/webox/internal/wechat"
	"github.com/netcat-ai/webox/wecom"
)

type ChatData struct {
	Sequence         uint64
	MessageID        string
	EncryptedKey     string
	EncryptedMessage string
}

type MediaData struct {
	NextIndex string
	Finished  bool
	Data      []byte
}

type Client interface {
	GetChatData(uint64, uint64) ([]ChatData, error)
	DecryptData(ChatData) ([]byte, error)
	GetMediaData(string, string) (*MediaData, error)
	Free()
}

type Config struct {
	AccountID       string
	WeChatID        string
	Nickname        string
	InitialSequence uint64
}

type Source struct {
	client          Client
	media           *sharedmedia.Store
	accountID       string
	wechatID        string
	nickname        string
	initialSequence uint64
}

func New(client Client, media *sharedmedia.Store, config Config) (*Source, error) {
	if client == nil {
		return nil, errors.New("WeCom archive client is required")
	}
	if media == nil {
		return nil, errors.New("shared media store is required")
	}
	if config.AccountID == "" || config.WeChatID == "" {
		return nil, errors.New("WeCom archive account_id and wechat_id are required")
	}
	return &Source{
		client:          client,
		media:           media,
		accountID:       config.AccountID,
		wechatID:        config.WeChatID,
		nickname:        config.Nickname,
		initialSequence: config.InitialSequence,
	}, nil
}

func (source *Source) IsInitialized() bool {
	return true
}

func (source *Source) UserInfo() (wechat.UserInfo, error) {
	return wechat.UserInfo{
		AccountID: source.accountID,
		WeChatID:  source.wechatID,
		Nickname:  source.nickname,
	}, nil
}

func (source *Source) GetUpdates(ctx context.Context, cursors map[string]int64, limit int) (map[string][]wecom.Message, error) {
	after := source.initialSequence
	for _, sequence := range cursors {
		if uint64(sequence) > after {
			after = uint64(sequence)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := source.client.GetChatData(after, uint64(limit))
	if err != nil {
		return nil, fmt.Errorf("get WeCom archive data after seq %d: %w", after, err)
	}

	updates := make(map[string][]wecom.Message)
	for _, encrypted := range data {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if encrypted.Sequence > math.MaxInt64 {
			return nil, fmt.Errorf("WeCom archive seq %d exceeds the public message range", encrypted.Sequence)
		}
		plain, err := source.client.DecryptData(encrypted)
		if err != nil {
			return nil, fmt.Errorf("decrypt WeCom archive message %s: %w", encrypted.MessageID, err)
		}
		message, err := source.convertMessage(plain, int64(encrypted.Sequence))
		if err != nil {
			return nil, fmt.Errorf("convert WeCom archive message %s: %w", encrypted.MessageID, err)
		}
		if cursor, known := cursors[message.RoomID]; known && message.Sequence <= cursor {
			continue
		}
		if err := source.materializeMedia(ctx, &message); err != nil {
			return nil, fmt.Errorf("materialize WeCom archive message %s: %w", message.MsgID, err)
		}
		updates[message.RoomID] = append(updates[message.RoomID], message)
	}
	return updates, nil
}

type archiveMessage struct {
	MsgID   string   `json:"msgid"`
	Action  string   `json:"action"`
	From    string   `json:"from"`
	ToList  []string `json:"tolist"`
	RoomID  string   `json:"roomid"`
	MsgTime int64    `json:"msgtime"`
	MsgType string   `json:"msgtype"`

	Text  *wecom.Text  `json:"text"`
	Image *wecom.Image `json:"image"`
	File  *wecom.File  `json:"file"`
	Link  *wecom.Link  `json:"link"`
}

func (source *Source) convertMessage(data []byte, sequence int64) (wecom.Message, error) {
	var archived archiveMessage
	if err := json.Unmarshal(data, &archived); err != nil {
		return wecom.Message{}, err
	}
	roomID, err := source.roomID(archived)
	if err != nil {
		return wecom.Message{}, err
	}
	message := wecom.Message{
		MsgID: archived.MsgID, Action: archived.Action, From: archived.From,
		ToList: archived.ToList, RoomID: roomID, MsgTime: archived.MsgTime,
		MsgType: archived.MsgType, Sequence: sequence,
	}
	if message.MsgID == "" {
		return wecom.Message{}, errors.New("msgid is required")
	}
	switch message.MsgType {
	case wecom.MessageTypeText:
		if archived.Text == nil {
			return wecom.Message{}, errors.New("text body is required")
		}
		message.Text = archived.Text
	case wecom.MessageTypeLink:
		if archived.Link == nil {
			return wecom.Message{}, errors.New("link body is required")
		}
		message.Link = archived.Link
	case wecom.MessageTypeImage:
		if archived.Image == nil {
			return wecom.Message{}, errors.New("image body is required")
		}
		message.Image = archived.Image
	case wecom.MessageTypeFile:
		if archived.File == nil {
			return wecom.Message{}, errors.New("file body is required")
		}
		message.File = archived.File
	default:
		return wecom.Message{}, fmt.Errorf("unsupported msgtype %q", message.MsgType)
	}
	return message, nil
}

func (source *Source) roomID(message archiveMessage) (string, error) {
	if message.RoomID != "" {
		roomID := message.RoomID
		if !strings.HasSuffix(roomID, "@chatroom") {
			roomID += "@chatroom"
		}
		return validRoomID(roomID)
	}
	if message.From == source.accountID {
		var peer string
		for _, recipient := range message.ToList {
			if recipient == source.accountID {
				continue
			}
			if peer != "" {
				return "", errors.New("direct message has multiple recipients")
			}
			peer = recipient
		}
		return validRoomID(peer)
	}
	for _, recipient := range message.ToList {
		if recipient == source.accountID {
			return validRoomID(message.From)
		}
	}
	return "", errors.New("direct message does not involve the configured account")
}

func validRoomID(roomID string) (string, error) {
	if roomID == "" || roomID == "." || roomID == ".." || strings.ContainsAny(roomID, "/\\\x00") {
		return "", errors.New("roomid is not a safe directory name")
	}
	return roomID, nil
}

func (source *Source) materializeMedia(ctx context.Context, message *wecom.Message) error {
	switch message.MsgType {
	case wecom.MessageTypeImage:
		data, err := source.downloadMedia(ctx, message.Image.SDKFileID)
		if err != nil {
			return err
		}
		if err := verifyMedia(data, message.Image.FileSize, message.Image.MD5Sum); err != nil {
			return err
		}
		path, err := source.media.WriteInbox(message.RoomID, message.MsgID, data, http.DetectContentType(data))
		if err != nil {
			return err
		}
		message.Image.SDKFileID = path
	case wecom.MessageTypeFile:
		data, err := source.downloadMedia(ctx, message.File.SDKFileID)
		if err != nil {
			return err
		}
		if err := verifyMedia(data, message.File.FileSize, message.File.MD5Sum); err != nil {
			return err
		}
		path, err := source.media.WriteInboxFile(message.RoomID, message.MsgID, message.File.FileName, data)
		if err != nil {
			return err
		}
		message.File.SDKFileID = path
	}
	return nil
}

func (source *Source) downloadMedia(ctx context.Context, sdkFileID string) ([]byte, error) {
	if sdkFileID == "" {
		return nil, errors.New("sdkfileid is required")
	}
	var data bytes.Buffer
	var index string
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk, err := source.client.GetMediaData(index, sdkFileID)
		if err != nil {
			return nil, err
		}
		if chunk == nil {
			return nil, errors.New("media response is empty")
		}
		_, _ = data.Write(chunk.Data)
		if chunk.Finished {
			return data.Bytes(), nil
		}
		index = chunk.NextIndex
	}
}

func verifyMedia(data []byte, size int64, md5sum string) error {
	if size != 0 && int64(len(data)) != size {
		return fmt.Errorf("media size is %d, expected %d", len(data), size)
	}
	if md5sum != "" {
		digest := md5.Sum(data)
		if !strings.EqualFold(hex.EncodeToString(digest[:]), md5sum) {
			return errors.New("media md5 does not match")
		}
	}
	return nil
}

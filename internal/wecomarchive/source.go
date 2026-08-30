package wecomarchive

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/netcat-ai/webox/internal/sharedmedia"
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
	Logger          *slog.Logger
}

type Source struct {
	client          Client
	media           *sharedmedia.Store
	accountID       string
	wechatID        string
	nickname        string
	initialSequence uint64
	logger          *slog.Logger
}

var errMessageNotForAccount = errors.New("message cannot be projected for the configured account")

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
	logger := config.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Source{
		client:          client,
		media:           media,
		accountID:       config.AccountID,
		wechatID:        config.WeChatID,
		nickname:        config.Nickname,
		initialSequence: config.InitialSequence,
		logger:          logger,
	}, nil
}

type Identity struct {
	AccountID string
	WeChatID  string
	Nickname  string
}

func (source *Source) Identity() Identity {
	return Identity{
		AccountID: source.accountID,
		WeChatID:  source.wechatID,
		Nickname:  source.nickname,
	}
}

func (source *Source) GetUpdates(ctx context.Context, after uint64, limit int) ([]json.RawMessage, uint64, error) {
	if after < source.initialSequence {
		after = source.initialSequence
	}
	if err := ctx.Err(); err != nil {
		return nil, after, err
	}
	data, err := source.client.GetChatData(after, uint64(limit))
	if err != nil {
		return nil, after, fmt.Errorf("get WeCom archive data after seq %d: %w", after, err)
	}

	updates := make([]json.RawMessage, 0, len(data))
	next := after
	for _, encrypted := range data {
		if err := ctx.Err(); err != nil {
			return nil, after, err
		}
		if encrypted.Sequence > next {
			next = encrypted.Sequence
		}
		plain, err := source.client.DecryptData(encrypted)
		if err != nil {
			return nil, after, fmt.Errorf("decrypt WeCom archive message %s: %w", encrypted.MessageID, err)
		}
		message, err := source.convertMessage(plain)
		if err != nil {
			if errors.Is(err, errMessageNotForAccount) {
				source.logger.Warn("skipping WeCom archive message", "message_id", encrypted.MessageID, "error", err)
				continue
			}
			return nil, after, fmt.Errorf("convert WeCom archive message %s: %w", encrypted.MessageID, err)
		}
		if err := source.materializeMedia(ctx, message); err != nil {
			return nil, after, fmt.Errorf("materialize WeCom archive message %s: %w", encrypted.MessageID, err)
		}
		encoded, err := json.Marshal(message)
		if err != nil {
			return nil, after, err
		}
		updates = append(updates, encoded)
	}
	return updates, next, nil
}

func (source *Source) convertMessage(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var message map[string]any
	if err := decoder.Decode(&message); err != nil {
		return nil, err
	}
	roomID, err := source.roomID(message)
	if err != nil {
		return nil, err
	}
	if stringField(message, "msgid") == "" {
		return nil, errors.New("msgid is required")
	}
	if stringField(message, "msgtype") == "" {
		return nil, errors.New("msgtype is required")
	}
	message["roomid"] = roomID
	return message, nil
}

func (source *Source) roomID(message map[string]any) (string, error) {
	if value := stringField(message, "roomid"); value != "" {
		roomID := value
		if !strings.HasSuffix(roomID, "@chatroom") {
			roomID += "@chatroom"
		}
		return validRoomID(roomID)
	}
	from := stringField(message, "from")
	recipients := stringSliceField(message, "tolist")
	if from == source.accountID {
		var peer string
		for _, recipient := range recipients {
			if recipient == source.accountID {
				continue
			}
			if peer != "" {
				return "", fmt.Errorf("%w: direct message has multiple recipients", errMessageNotForAccount)
			}
			peer = recipient
		}
		roomID, err := validRoomID(peer)
		if err != nil {
			return "", fmt.Errorf("%w: %v", errMessageNotForAccount, err)
		}
		return roomID, nil
	}
	for _, recipient := range recipients {
		if recipient == source.accountID {
			return validRoomID(from)
		}
	}
	return "", fmt.Errorf("%w: direct message does not involve the configured account", errMessageNotForAccount)
}

func stringField(message map[string]any, name string) string {
	value, _ := message[name].(string)
	return value
}

func stringSliceField(message map[string]any, name string) []string {
	values, _ := message[name].([]any)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func validRoomID(roomID string) (string, error) {
	if roomID == "" || roomID == "." || roomID == ".." || strings.ContainsAny(roomID, "/\\\x00") {
		return "", errors.New("roomid is not a safe directory name")
	}
	return roomID, nil
}

func (source *Source) materializeMedia(ctx context.Context, message map[string]any) error {
	msgType := stringField(message, "msgtype")
	roomID := stringField(message, "roomid")
	messageID := stringField(message, "msgid")
	mediaIndex := 0
	for _, field := range mediaFields(msgType) {
		body, ok := message[field]
		if !ok {
			continue
		}
		if field == "mixed" {
			if err := source.materializeValue(ctx, roomID, messageID, body, &mediaIndex); err != nil {
				return err
			}
			continue
		}
		if bodyMap, ok := body.(map[string]any); ok {
			if err := source.materializeBody(ctx, roomID, messageID, bodyMap, &mediaIndex); err != nil {
				return err
			}
		}
	}
	return nil
}

func mediaFields(msgType string) []string {
	switch msgType {
	case "image", "voice", "video", "file", "emotion":
		return []string{msgType}
	case "mixed":
		return []string{"mixed"}
	default:
		return nil
	}
}

func (source *Source) materializeBody(ctx context.Context, roomID, messageID string, body map[string]any, mediaIndex *int) error {
	remote, _ := body["sdkfileid"].(string)
	if remote == "" {
		return nil
	}
	*mediaIndex++
	data, err := source.downloadMedia(ctx, remote)
	if err != nil {
		return err
	}
	if err := verifyMedia(data, archivedMediaSize(body), archivedMediaMD5(body)); err != nil {
		return err
	}
	path, err := source.writeArchivedMedia(roomID, messageID+"-"+strconv.Itoa(*mediaIndex), body, data)
	if err != nil {
		return err
	}
	body["sdkfileid"] = path
	return nil
}

func (source *Source) materializeValue(ctx context.Context, roomID, messageID string, value any, mediaIndex *int) error {
	switch current := value.(type) {
	case []any:
		for _, item := range current {
			if err := source.materializeValue(ctx, roomID, messageID, item, mediaIndex); err != nil {
				return err
			}
		}
	case map[string]any:
		if err := source.materializeBody(ctx, roomID, messageID, current, mediaIndex); err != nil {
			return err
		}
		for key, value := range current {
			if key != "sdkfileid" {
				if err := source.materializeValue(ctx, roomID, messageID, value, mediaIndex); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (source *Source) writeArchivedMedia(roomID, messageID string, body map[string]any, data []byte) (string, error) {
	if filename, ok := body["filename"].(string); ok && filename != "" {
		return source.media.WriteInboxFile(roomID, messageID, filename, data)
	}
	contentType := http.DetectContentType(data)
	if strings.HasPrefix(contentType, "image/") {
		if path, err := source.media.WriteInbox(roomID, messageID, data, contentType); err == nil {
			return path, nil
		}
	}
	return source.media.WriteInboxFile(roomID, messageID, "media.bin", data)
}

func archivedMediaMD5(body map[string]any) string {
	checksum, _ := body["md5sum"].(string)
	return checksum
}

func archivedMediaSize(body map[string]any) int64 {
	for _, field := range []string{"filesize", "imagesize", "voice_size", "videosize"} {
		if number, ok := body[field].(json.Number); ok {
			size, err := number.Int64()
			if err == nil {
				return size
			}
		}
	}
	return 0
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

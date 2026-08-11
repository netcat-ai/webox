package ilink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/netcat-ai/webox/internal/sender"
	"github.com/netcat-ai/webox/internal/sharedmedia"
	"github.com/netcat-ai/webox/internal/wechat"
	"github.com/netcat-ai/webox/internal/wechatdb"
	"github.com/netcat-ai/webox/wecom"
)

const (
	getUpdatesTimeout  = 35 * time.Second
	getUpdatesInterval = time.Second
	getUpdatesLimit    = 100
	inboundMediaWait   = time.Minute
)

type messageSource interface {
	IsInitialized() bool
	UserInfo() (wechat.UserInfo, error)
	ContactsByRemark(string) ([]wechatdb.Contact, error)
	RoomSessions() (map[string]int64, error)
	RoomMessages(string, int64, int) ([]wecom.Message, error)
	ReadImage(string, int64, int64, string) (*wechatdb.MediaFile, error)
	ReadFile(string, int64, int64, string) (*wechatdb.LocalFile, error)
}

type messageSender interface {
	Send(context.Context, string, []sender.Item) (sender.Receipt, error)
}

type Server struct {
	apiToken string
	media    *sharedmedia.Store
	messages messageSource
	sender   messageSender
	logger   *slog.Logger

	sendMu sync.Mutex

	pollTimeout  time.Duration
	pollInterval time.Duration
}

type roomCursor struct {
	Sequence int64 `json:"seq"`
}

type getUpdatesRequest struct {
	Rooms map[string]roomCursor `json:"rooms"`
}

type roomUpdates struct {
	Messages []wecom.Message `json:"messages"`
}

type sendMessageRequest struct {
	Messages []wecom.Message `json:"msgs"`
}

func New(apiToken string, media *sharedmedia.Store, messages messageSource, sender messageSender, logger *slog.Logger) *Server {
	return &Server{
		apiToken:     apiToken,
		media:        media,
		messages:     messages,
		sender:       sender,
		logger:       logger,
		pollTimeout:  getUpdatesTimeout,
		pollInterval: getUpdatesInterval,
	}
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("GET /ilink/bot/userinfo", server.getUserInfo)
	mux.HandleFunc("GET /ilink/bot/contacts", server.getContacts)
	mux.HandleFunc("POST /ilink/bot/getupdates", server.getUpdates)
	mux.HandleFunc("POST /ilink/bot/sendmessage", server.sendMessage)
	return mux
}

func (server *Server) getUpdates(response http.ResponseWriter, request *http.Request) {
	if !server.authenticate(response, request) {
		return
	}
	var body getUpdatesRequest
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	for roomID, cursor := range body.Rooms {
		if roomID == "" {
			writeError(response, http.StatusBadRequest, "roomid must not be empty")
			return
		}
		if cursor.Sequence < 0 {
			writeError(response, http.StatusBadRequest, "room seq must not be negative")
			return
		}
	}
	if !server.messages.IsInitialized() {
		writeJSON(response, http.StatusOK, sessionUnavailable())
		return
	}

	deadline := time.Now().Add(server.pollTimeout)
	for {
		updates, err := server.updatedRooms(body.Rooms)
		if err != nil {
			server.logger.Warn("WeChat room polling failed", "error", err)
			writeJSON(response, http.StatusOK, sessionUnavailable())
			return
		}
		if len(updates) != 0 || !time.Now().Before(deadline) {
			writeJSON(response, http.StatusOK, map[string]any{
				"ret": 0, "rooms": updates,
				"longpolling_timeout_ms": server.pollTimeout.Milliseconds(),
			})
			return
		}
		if !waitRequest(request.Context(), server.pollInterval) {
			return
		}
		if !server.messages.IsInitialized() {
			writeJSON(response, http.StatusOK, sessionUnavailable())
			return
		}
	}
}

func (server *Server) updatedRooms(cursors map[string]roomCursor) (map[string]roomUpdates, error) {
	sessions, err := server.messages.RoomSessions()
	if err != nil {
		return nil, err
	}
	roomIDs := make([]string, 0, len(sessions))
	for roomID := range sessions {
		roomIDs = append(roomIDs, roomID)
	}
	sort.Strings(roomIDs)

	updates := make(map[string]roomUpdates)
	for _, roomID := range roomIDs {
		latest := sessions[roomID]
		cursor, known := cursors[roomID]
		after := cursor.Sequence
		if !known {
			after = max(0, latest-getUpdatesLimit)
		}
		if latest <= after {
			continue
		}
		messages, err := server.messages.RoomMessages(roomID, after, getUpdatesLimit)
		if err != nil {
			server.logger.Warn("could not read WeChat Room messages", "roomid", roomID, "seq", after, "error", err)
			continue
		}
		ready, err := server.readyRoomMessages(roomID, messages)
		if err != nil {
			server.logger.Warn("could not materialize WeChat Room message", "roomid", roomID, "error", err)
		}
		if len(ready) != 0 {
			updates[roomID] = roomUpdates{Messages: ready}
		}
	}
	return updates, nil
}

func (server *Server) readyRoomMessages(roomID string, messages []wecom.Message) ([]wecom.Message, error) {
	ready := make([]wecom.Message, 0, len(messages))
	now := time.Now()
	for _, message := range messages {
		var err error
		switch message.MsgType {
		case wecom.MessageTypeImage:
			message.Image.SDKFileID, err = server.materializeInboundImage(roomID, message)
		case wecom.MessageTypeFile:
			var file *wechatdb.LocalFile
			file, message.File.SDKFileID, err = server.materializeInboundFile(roomID, message)
			if err == nil {
				message.File.FileName = file.Filename
			}
		}
		if err != nil {
			if !errors.Is(err, errInboundMediaNotReady) {
				return ready, fmt.Errorf("materialize WeChat message %s: %w", message.MsgID, err)
			}
			if !inboundMediaWaitExpired(message, now) {
				server.logger.Debug("waiting for WeChat media", "roomid", roomID, "msgid", message.MsgID, "type", message.MsgType)
				break
			}
			server.logger.Warn("delivering WeChat media without sdkfileid after waiting one minute", "roomid", roomID, "msgid", message.MsgID, "type", message.MsgType)
		}
		ready = append(ready, message)
	}
	return ready, nil
}

func (server *Server) health(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]any{"ok": true, "ready": server.messages.IsInitialized()})
}

func (server *Server) getUserInfo(response http.ResponseWriter, request *http.Request) {
	if !server.authenticate(response, request) {
		return
	}
	if !server.messages.IsInitialized() {
		writeJSON(response, http.StatusOK, sessionUnavailable())
		return
	}
	info, err := server.messages.UserInfo()
	if err != nil {
		server.logger.Warn("could not read current WeChat account", "error", err)
		writeJSON(response, http.StatusOK, sessionUnavailable())
		return
	}
	accountID := info.AccountID
	wechatID := info.WeChatID
	if accountID == "" || wechatID == "" {
		server.logger.Warn("current WeChat account identity is incomplete")
		writeJSON(response, http.StatusOK, sessionUnavailable())
		return
	}
	result := map[string]any{
		"ret":        0,
		"account_id": accountID,
		"wechat_id":  wechatID,
	}
	if nickname := strings.TrimSpace(info.Nickname); nickname != "" {
		result["nickname"] = nickname
	}
	if avatarURL := strings.TrimSpace(info.AvatarURL); avatarURL != "" {
		result["avatar_url"] = avatarURL
	}
	writeJSON(response, http.StatusOK, result)
}

func (server *Server) getContacts(response http.ResponseWriter, request *http.Request) {
	if !server.authenticate(response, request) {
		return
	}
	remark := request.URL.Query().Get("remark")
	if remark == "" {
		writeError(response, http.StatusBadRequest, "remark is required")
		return
	}
	if !server.messages.IsInitialized() {
		writeJSON(response, http.StatusOK, sessionUnavailable())
		return
	}
	contacts, err := server.messages.ContactsByRemark(remark)
	if err != nil {
		server.logger.Error("could not query WeChat contacts", "remark", remark, "error", err)
		writeError(response, http.StatusInternalServerError, "could not query WeChat contacts")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"ret": 0, "contacts": contacts})
}

func (server *Server) sendMessage(response http.ResponseWriter, request *http.Request) {
	if !server.authenticate(response, request) {
		return
	}
	var body sendMessageRequest
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	if !server.messages.IsInitialized() {
		writeJSON(response, http.StatusOK, sessionUnavailable())
		return
	}
	target, items, err := outboundMessages(body.Messages)
	if errors.Is(err, errUnsupportedOutboundItem) {
		writeError(response, http.StatusNotImplemented, err.Error())
		return
	}
	if err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	server.sendMu.Lock()
	defer server.sendMu.Unlock()
	receipt, sendErr := server.sender.Send(request.Context(), target, items)
	if sendErr != nil {
		server.logger.Error("could not send WeChat message", "target", target, "error", sendErr)
		writeError(response, http.StatusInternalServerError, sendErr.Error())
		return
	}
	resultID := receipt.ClientMessageID
	if len(body.Messages) != 0 && body.Messages[0].MsgID != "" {
		resultID = body.Messages[0].MsgID
	}
	server.logger.Info("WeChat message sent", "target", target, "client_msg_id", resultID)
	writeJSON(response, http.StatusOK, sendSuccess(resultID))
}

func (server *Server) authenticate(response http.ResponseWriter, request *http.Request) bool {
	if !strings.EqualFold(strings.TrimSpace(request.Header.Get("AuthorizationType")), "ilink_bot_token") {
		writeError(response, http.StatusUnauthorized, "missing or invalid AuthorizationType")
		return false
	}
	if strings.TrimSpace(request.Header.Get("X-WECHAT-UIN")) == "" {
		writeError(response, http.StatusUnauthorized, "missing X-WECHAT-UIN")
		return false
	}
	if strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")) != server.apiToken || !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") {
		writeError(response, http.StatusUnauthorized, "missing or invalid bearer token")
		return false
	}
	return true
}

func decodeJSON(request *http.Request, target any) error {
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func writeJSON(response http.ResponseWriter, status int, body any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}

func writeError(response http.ResponseWriter, status int, detail string) {
	writeJSON(response, status, map[string]any{"error": http.StatusText(status), "detail": detail})
}

func sessionUnavailable() map[string]any {
	return map[string]any{"ret": -14, "errcode": -14, "errmsg": "wechat session is unavailable", "rooms": map[string]any{}}
}

func sendSuccess(clientMessageID string) map[string]any {
	return map[string]any{"ret": 0, "client_msg_id": clientMessageID}
}

var errUnsupportedOutboundItem = errors.New("only text, image, and file sending are supported")

func outboundMessages(messages []wecom.Message) (string, []sender.Item, error) {
	if len(messages) == 0 {
		return "", nil, errors.New("msgs is required")
	}
	items := make([]sender.Item, 0, len(messages))
	var target string
	for _, message := range messages {
		messageTarget, err := outboundMessageTarget(message)
		if err != nil {
			return "", nil, err
		}
		if target == "" {
			target = messageTarget
		} else if target != messageTarget {
			return "", nil, errors.New("all msgs must have the same roomid")
		}
		switch message.MsgType {
		case wecom.MessageTypeText:
			if message.Text == nil || strings.TrimSpace(message.Text.Content) == "" {
				return "", nil, errors.New("text.content is required")
			}
			items = append(items, sender.Item{Kind: "text", Text: message.Text.Content})
		case wecom.MessageTypeImage:
			if message.Image == nil || message.Image.SDKFileID == "" {
				return "", nil, errors.New("image.sdkfileid is required")
			}
			items = append(items, sender.Item{Kind: "image", SharedPath: message.Image.SDKFileID})
		case wecom.MessageTypeFile:
			if message.File == nil || message.File.SDKFileID == "" {
				return "", nil, errors.New("file.sdkfileid is required")
			}
			items = append(items, sender.Item{Kind: "file", SharedPath: message.File.SDKFileID})
		default:
			return "", nil, errUnsupportedOutboundItem
		}
	}
	return target, items, nil
}

func outboundMessageTarget(message wecom.Message) (string, error) {
	if len(message.ToList) != 0 {
		return "", errors.New("tolist must be empty")
	}
	target := message.RoomID
	if target == "" {
		return "", errors.New("roomid is required")
	}
	return target, nil
}

func waitRequest(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

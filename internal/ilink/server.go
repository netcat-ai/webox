package ilink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	inboundMediaWait   = time.Minute
)

type messageSource interface {
	IsInitialized() bool
	UserInfo() (wechat.UserInfo, error)
	ValidatePollCursor(string) error
	PollMessages(string, int) (wechat.PollResult, error)
	ReadImage(string, string) (*wechatdb.MediaFile, error)
	ReadFile(string, string) (*wechatdb.LocalFile, error)
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

type getUpdatesRequest struct {
	Cursor string `json:"get_updates_buf"`
}

type sendMessageRequest struct {
	Messages []wecom.Message `json:"msgs"`
}

func New(apiToken string, media *sharedmedia.Store, messages messageSource, sender messageSender, logger *slog.Logger) *Server {
	return &Server{
		apiToken:    apiToken,
		media:       media,
		messages:    messages,
		sender:      sender,
		logger:      logger,
		pollTimeout: getUpdatesTimeout, pollInterval: getUpdatesInterval,
	}
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("GET /ilink/bot/userinfo", server.getUserInfo)
	mux.HandleFunc("POST /ilink/bot/getupdates", server.getUpdates)
	mux.HandleFunc("POST /ilink/bot/sendmessage", server.sendMessage)
	return mux
}

func (server *Server) health(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]any{"ok": true, "ready": server.messages.IsInitialized()})
}

func (server *Server) getUserInfo(response http.ResponseWriter, request *http.Request) {
	if !server.authenticate(response, request) {
		return
	}
	if !server.messages.IsInitialized() {
		writeJSON(response, http.StatusOK, sessionUnavailable(""))
		return
	}
	info, err := server.messages.UserInfo()
	if err != nil {
		server.logger.Warn("could not read current WeChat account", "error", err)
		writeJSON(response, http.StatusOK, sessionUnavailable(""))
		return
	}
	accountID := strings.TrimSpace(info.AccountID)
	wechatID := strings.TrimSpace(info.WeChatID)
	if accountID == "" || wechatID == "" {
		server.logger.Warn("current WeChat account identity is incomplete")
		writeJSON(response, http.StatusOK, sessionUnavailable(""))
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

func (server *Server) getUpdates(response http.ResponseWriter, request *http.Request) {
	if !server.authenticate(response, request) {
		return
	}
	var body getUpdatesRequest
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	if err := server.messages.ValidatePollCursor(body.Cursor); err != nil {
		writeError(response, http.StatusBadRequest, "invalid get_updates_buf: "+err.Error())
		return
	}
	if !server.messages.IsInitialized() {
		writeJSON(response, http.StatusOK, sessionUnavailable(body.Cursor))
		return
	}
	cursor := body.Cursor
	deadline := time.Now().Add(server.pollTimeout)
	for {
		result, err := server.messages.PollMessages(cursor, 100)
		if err != nil {
			server.logger.Warn("WeChat message polling failed", "error", err)
			writeJSON(response, http.StatusOK, sessionUnavailable(cursor))
			return
		}
		accountID := strings.TrimSpace(result.AccountID)
		if accountID == "" {
			server.logger.Warn("WeChat message polling returned no account identity")
			writeJSON(response, http.StatusOK, sessionUnavailable(cursor))
			return
		}
		if len(result.Messages) == 0 {
			cursor = result.Cursor
			server.logSkippedMessages(result.Skipped)
		} else {
			messages := make([]wecom.Message, 0, len(result.Messages))
			blocked := false
			for _, message := range result.Messages {
				mediaWaitExpired := inboundMediaWaitExpired(message, time.Now())
				view, materializeErr := server.prepareInboundMessage(message, accountID, mediaWaitExpired)
				if errors.Is(materializeErr, errInboundMediaNotReady) {
					server.logger.Debug("waiting for WeChat media",
						"msgid", message.MsgID, "type", message.MsgType, "error", materializeErr,
					)
					blocked = true
					break
				}
				if materializeErr != nil {
					server.logger.Warn("could not materialize WeChat message", "error", materializeErr)
					writeError(response, http.StatusInternalServerError, materializeErr.Error())
					return
				}
				if mediaWaitExpired && hasMissingSharedMedia(view) {
					server.logger.Warn("delivering WeChat media without sdkfileid after waiting one minute",
						"msgid", message.MsgID, "type", message.MsgType,
					)
				}
				messages = append(messages, view)
			}
			if !blocked {
				cursor = result.Cursor
				server.logSkippedMessages(result.Skipped)
				writeJSON(response, http.StatusOK, map[string]any{
					"ret": 0, "msgs": messages, "get_updates_buf": cursor,
					"longpolling_timeout_ms": server.pollTimeout.Milliseconds(),
				})
				return
			}
		}
		if !time.Now().Before(deadline) {
			writeJSON(response, http.StatusOK, map[string]any{
				"ret": 0, "msgs": []map[string]any{}, "get_updates_buf": cursor,
				"longpolling_timeout_ms": server.pollTimeout.Milliseconds(),
			})
			return
		}
		if !waitRequest(request.Context(), server.pollInterval) {
			return
		}
		if !server.messages.IsInitialized() {
			writeJSON(response, http.StatusOK, sessionUnavailable(cursor))
			return
		}
	}
}

func (server *Server) logSkippedMessages(messages []wechatdb.SkippedMessage) {
	for _, message := range messages {
		server.logger.Warn("skipping WeChat message with unresolved sender",
			"msgid", message.MessageID,
			"message_shard", message.Shard,
			"real_sender_id", message.RealSenderID,
		)
	}
}

func inboundMediaWaitExpired(message wecom.Message, now time.Time) bool {
	createdAt := message.MsgTime
	return createdAt <= 0 || !now.Before(time.UnixMilli(createdAt).Add(inboundMediaWait))
}

func (server *Server) prepareInboundMessage(message wecom.Message, accountID string, allowMissingMedia bool) (wecom.Message, error) {
	view := cloneMessage(message)
	if view.Outgoing {
		view.From = accountID
	}
	view.ToList = []string{}
	if err := server.materializeInboundMessage(view.RoomID, &view, allowMissingMedia); err != nil {
		return wecom.Message{}, err
	}
	return view, nil
}

func cloneMessage(message wecom.Message) wecom.Message {
	message.ToList = append([]string(nil), message.ToList...)
	if message.Text != nil {
		value := *message.Text
		message.Text = &value
	}
	if message.Image != nil {
		value := *message.Image
		message.Image = &value
	}
	if message.Voice != nil {
		value := *message.Voice
		message.Voice = &value
	}
	if message.File != nil {
		value := *message.File
		message.File = &value
	}
	if message.Video != nil {
		value := *message.Video
		message.Video = &value
	}
	if message.Link != nil {
		value := *message.Link
		message.Link = &value
	}
	if message.SphFeed != nil {
		value := *message.SphFeed
		message.SphFeed = &value
	}
	if message.Mixed != nil {
		value := *message.Mixed
		value.Items = append([]wecom.MixedItem(nil), value.Items...)
		message.Mixed = &value
	}
	return message
}

func (server *Server) materializeInboundMessage(roomID string, message *wecom.Message, allowMissingMedia bool) error {
	var err error
	switch message.MsgType {
	case wecom.MessageTypeImage:
		if message.Image == nil {
			message.Image = &wecom.Image{}
		}
		message.Image.SDKFileID, err = server.materializeInboundImage(roomID, message.MsgID)
	case wecom.MessageTypeFile:
		if message.File == nil {
			message.File = &wecom.File{}
		}
		var file *wechatdb.LocalFile
		file, message.File.SDKFileID, err = server.materializeInboundFile(roomID, message.MsgID)
		if err == nil {
			message.File.FileName = file.Filename
		}
	case wecom.MessageTypeMixed:
		if message.Mixed != nil {
			for index := range message.Mixed.Items {
				item := &message.Mixed.Items[index]
				if item.Type != wecom.MessageTypeImage {
					continue
				}
				item.SDKFileID, err = server.materializeInboundImage(roomID, item.MessageID)
				if err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		if !(allowMissingMedia && errors.Is(err, errInboundMediaNotReady)) {
			return err
		}
	}
	return nil
}

func hasMissingSharedMedia(message wecom.Message) bool {
	if message.MsgType == wecom.MessageTypeImage && (message.Image == nil || strings.TrimSpace(message.Image.SDKFileID) == "") {
		return true
	}
	if message.MsgType == wecom.MessageTypeFile && (message.File == nil || strings.TrimSpace(message.File.SDKFileID) == "") {
		return true
	}
	if message.MsgType == wecom.MessageTypeMixed && message.Mixed != nil {
		for _, item := range message.Mixed.Items {
			if item.Type == wecom.MessageTypeImage && strings.TrimSpace(item.SDKFileID) == "" {
				return true
			}
		}
	}
	return false
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
		writeJSON(response, http.StatusOK, sessionUnavailable(""))
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
	if len(body.Messages) != 0 && strings.TrimSpace(body.Messages[0].MsgID) != "" {
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

func sessionUnavailable(cursor string) map[string]any {
	result := map[string]any{"ret": -14, "errcode": -14, "errmsg": "wechat session is unavailable"}
	if cursor != "" {
		result["msgs"] = []any{}
		result["get_updates_buf"] = cursor
	}
	return result
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
			items = append(items, sender.Item{Kind: "text", Text: strings.TrimSpace(message.Text.Content)})
		case wecom.MessageTypeImage:
			if message.Image == nil || strings.TrimSpace(message.Image.SDKFileID) == "" {
				return "", nil, errors.New("image.sdkfileid is required")
			}
			items = append(items, sender.Item{Kind: "image", SharedPath: strings.TrimSpace(message.Image.SDKFileID)})
		case wecom.MessageTypeFile:
			if message.File == nil || strings.TrimSpace(message.File.SDKFileID) == "" {
				return "", nil, errors.New("file.sdkfileid is required")
			}
			items = append(items, sender.Item{Kind: "file", SharedPath: strings.TrimSpace(message.File.SDKFileID)})
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
	target := strings.TrimSpace(message.RoomID)
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

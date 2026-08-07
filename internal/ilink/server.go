package ilink

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/netcat-ai/webox/internal/qrsource"
	"github.com/netcat-ai/webox/internal/sender"
	"github.com/netcat-ai/webox/internal/sharedmedia"
	"github.com/netcat-ai/webox/internal/signedpayload"
	"github.com/netcat-ai/webox/internal/wechat"
	"github.com/netcat-ai/webox/internal/wechatdb"
)

const (
	textItemType       = 1
	getUpdatesTimeout  = 35 * time.Second
	getUpdatesInterval = time.Second
	qrAcquireTimeout   = 20 * time.Second
	qrSessionTTL       = 5 * time.Minute
	inboundMediaWait   = time.Minute
)

type messageSource interface {
	IsInitialized() bool
	AccountID() (string, error)
	UserInfo() (wechat.UserInfo, error)
	RefreshLoginQRCode() (bool, error)
	ValidatePollCursor(string) error
	PollMessages(string, int) (wechat.PollResult, error)
	ReadImage(string, string) (*wechatdb.MediaFile, error)
	ReadFile(string, string) (*wechatdb.LocalFile, error)
}

type messageSender interface {
	Send(context.Context, string, []sender.Item) (sender.Receipt, error)
}

type qrSource interface {
	Latest() (*qrsource.LoginCode, error)
}

type Server struct {
	apiToken      string
	publicBaseURL string
	media         *sharedmedia.Store
	messages      messageSource
	sender        messageSender
	qr            qrSource
	logger        *slog.Logger

	loginMu sync.Mutex
	login   loginSession
	sendMu  sync.Mutex

	pollTimeout  time.Duration
	pollInterval time.Duration
	qrTimeout    time.Duration
}

type loginSession struct {
	activeQrcode    string
	activeIssuedAt  time.Time
	confirmedQrcode string
}

type getUpdatesRequest struct {
	Cursor string `json:"get_updates_buf"`
}

type sendMessageRequest struct {
	Message outboundMessage `json:"msg"`
}

type outboundMessage struct {
	ClientID     string           `json:"client_id"`
	ContextToken string           `json:"context_token"`
	Items        []map[string]any `json:"item_list"`
}

type contextToken struct {
	Target string `json:"target"`
}

type typingTicket struct {
	UserID string `json:"ilink_user_id"`
}

func New(apiToken, publicBaseURL string, media *sharedmedia.Store, messages messageSource, sender messageSender, qr qrSource, logger *slog.Logger) *Server {
	return &Server{
		apiToken:      apiToken,
		publicBaseURL: strings.TrimRight(strings.TrimSpace(publicBaseURL), "/"),
		media:         media, messages: messages, sender: sender, qr: qr, logger: logger,
		pollTimeout: getUpdatesTimeout, pollInterval: getUpdatesInterval, qrTimeout: qrAcquireTimeout,
	}
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("GET /ilink/bot/get_bot_qrcode", server.getBotQrcode)
	mux.HandleFunc("POST /ilink/bot/get_bot_qrcode", server.getBotQrcode)
	mux.HandleFunc("GET /ilink/bot/get_qrcode_status", server.getQrcodeStatus)
	mux.HandleFunc("GET /ilink/bot/userinfo", server.getUserInfo)
	mux.HandleFunc("POST /ilink/bot/getupdates", server.getUpdates)
	mux.HandleFunc("POST /ilink/bot/sendmessage", server.sendMessage)
	mux.HandleFunc("POST /ilink/bot/getconfig", server.getConfig)
	mux.HandleFunc("POST /ilink/bot/sendtyping", server.sendTyping)
	mux.HandleFunc("POST /ilink/bot/msg/notifystart", server.notifyConnection)
	mux.HandleFunc("POST /ilink/bot/msg/notifystop", server.notifyConnection)
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

func (server *Server) getBotQrcode(response http.ResponseWriter, request *http.Request) {
	if value := strings.TrimSpace(request.URL.Query().Get("bot_type")); value != "" && value != "3" {
		writeError(response, http.StatusBadRequest, "unsupported bot_type")
		return
	}
	var body struct {
		LocalTokens []string `json:"local_token_list"`
	}
	if request.Method == http.MethodPost && request.Body != nil {
		if err := decodeJSON(request, &body); err != nil {
			writeError(response, http.StatusBadRequest, err.Error())
			return
		}
	}
	if server.messages.IsInitialized() {
		if !containsToken(body.LocalTokens, server.apiToken) {
			writeError(response, http.StatusUnauthorized, "WeChat is already logged in; a matching local token is required")
			return
		}
		server.loginMu.Lock()
		qrcode := server.login.registerResume()
		server.loginMu.Unlock()
		writeJSON(response, http.StatusOK, map[string]any{"qrcode": qrcode, "qrcode_img_content": ""})
		return
	}
	server.loginMu.Lock()
	expired := server.login.expired(time.Now())
	server.loginMu.Unlock()
	if expired {
		refreshed, err := server.messages.RefreshLoginQRCode()
		if err != nil {
			server.logger.Warn("could not refresh expired WeChat QR code", "error", err)
		} else if refreshed && !waitRequest(request.Context(), 750*time.Millisecond) {
			return
		}
	}

	deadline := time.Now().Add(server.qrTimeout)
	for {
		code, err := server.qr.Latest()
		if err != nil {
			server.logger.Warn("could not inspect WeChat login QR code", "error", err)
		}
		if code != nil {
			server.loginMu.Lock()
			qrcode := server.login.register(code.ID)
			server.loginMu.Unlock()
			writeJSON(response, http.StatusOK, map[string]any{"qrcode": qrcode, "qrcode_img_content": code.LoginURL})
			return
		}
		if time.Now().After(deadline) {
			writeError(response, http.StatusServiceUnavailable, "WeChat login QR code is not ready")
			return
		}
		if !waitRequest(request.Context(), 500*time.Millisecond) {
			return
		}
	}
}

func (server *Server) getQrcodeStatus(response http.ResponseWriter, request *http.Request) {
	qrcode := strings.TrimSpace(request.URL.Query().Get("qrcode"))
	if qrcode == "" {
		writeError(response, http.StatusBadRequest, "qrcode is required")
		return
	}
	current, err := server.qr.Latest()
	if err != nil {
		server.logger.Warn("could not inspect WeChat login QR code", "error", err)
	}
	currentID := ""
	if current != nil {
		currentID = current.ID
	}
	server.loginMu.Lock()
	status := server.login.status(qrcode, currentID, server.messages.IsInitialized(), time.Now())
	server.loginMu.Unlock()
	result := map[string]any{"status": status}
	if status == "confirmed" {
		accountID, err := server.messages.AccountID()
		if err != nil {
			writeError(response, http.StatusServiceUnavailable, "current WeChat account is unavailable")
			return
		}
		result["bot_token"] = server.apiToken
		result["ilink_bot_id"] = accountID
		result["ilink_user_id"] = accountID
		result["baseurl"] = server.baseURL(request)
	}
	writeJSON(response, http.StatusOK, result)
}

func (session *loginSession) register(qrcode string) string {
	if session.activeQrcode != qrcode {
		session.activeQrcode = qrcode
		session.activeIssuedAt = time.Now()
		session.confirmedQrcode = ""
	}
	return qrcode
}

func (session *loginSession) registerResume() string {
	return session.register("resume-" + randomID())
}

func (session *loginSession) expired(now time.Time) bool {
	return session.activeQrcode != "" && !session.activeIssuedAt.IsZero() && now.Sub(session.activeIssuedAt) >= qrSessionTTL
}

func (session *loginSession) status(requested, current string, initialized bool, now time.Time) string {
	known := requested == session.activeQrcode || requested == session.confirmedQrcode
	if !known {
		return "expired"
	}
	if initialized {
		session.confirmedQrcode = requested
		session.activeQrcode = ""
		session.activeIssuedAt = time.Time{}
		return "confirmed"
	}
	if requested == session.confirmedQrcode || session.activeIssuedAt.IsZero() || session.expired(now) {
		return "expired"
	}
	if current != "" {
		if current == requested {
			return "wait"
		}
		return "expired"
	}
	return "scaned"
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
		} else {
			messages := make([]map[string]any, 0, len(result.Messages))
			blocked := false
			for _, message := range result.Messages {
				mediaWaitExpired := inboundMediaWaitExpired(message, time.Now())
				view, materializeErr := server.standardMessage(message, accountID, mediaWaitExpired)
				if errors.Is(materializeErr, errInboundMediaNotReady) {
					server.logger.Debug("waiting for WeChat media",
						"msgid", stringValue(message["msgid"]), "type", messageType(message), "error", materializeErr,
					)
					blocked = true
					break
				}
				if materializeErr != nil {
					server.logger.Warn("could not materialize WeChat message", "error", materializeErr)
					writeError(response, http.StatusInternalServerError, materializeErr.Error())
					return
				}
				if mediaWaitExpired && (messageType(message) == "image" || messageType(message) == "file") {
					if _, available := view["shared_path"]; !available {
						server.logger.Warn("delivering WeChat media without shared_path after waiting one minute",
							"msgid", stringValue(message["msgid"]), "type", messageType(message),
						)
					}
				}
				messages = append(messages, view)
			}
			if !blocked {
				cursor = result.Cursor
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

func inboundMediaWaitExpired(message map[string]any, now time.Time) bool {
	createdAt := integerValue(message["msgtime"])
	return createdAt <= 0 || !now.Before(time.UnixMilli(createdAt).Add(inboundMediaWait))
}

func (server *Server) standardMessage(message map[string]any, accountID string, allowMissingMedia bool) (map[string]any, error) {
	externalID := stringValue(message["msgid"])
	createdAt := integerValue(message["msgtime"])
	roomID := stringValue(message["roomid"])
	if externalID == "" {
		externalID = strconv.FormatInt(stablePositiveID(fmt.Sprintf("%s:%d:%d", roomID, createdAt, integerValue(message["local_id"]))), 10)
	}
	messageID, err := strconv.ParseInt(externalID, 10, 64)
	if err != nil {
		messageID = stablePositiveID(externalID)
	}
	text := messageDisplayText(message)
	senderID := stringValue(message["from"])
	items := []map[string]any{{
		"type": textItemType, "create_time_ms": createdAt, "is_completed": true,
		"msg_id": externalID, "text_item": map[string]any{"text": text},
	}}
	sharedPath := ""
	kind := messageType(message)
	if kind == "image" {
		var imageErr error
		sharedPath, imageErr = server.materializeInboundImage(roomID, externalID)
		if imageErr != nil && !(allowMissingMedia && errors.Is(imageErr, errInboundMediaNotReady)) {
			return nil, imageErr
		}
		imageItem := map[string]any{}
		if sharedPath != "" {
			imageItem["shared_path"] = sharedPath
		}
		items = []map[string]any{{
			"type": imageItemType, "create_time_ms": createdAt, "is_completed": true,
			"msg_id": externalID, "image_item": imageItem,
		}}
	} else if kind == "file" {
		body, _ := message["file"].(map[string]any)
		filename := strings.TrimSpace(stringValue(body["filename"]))
		file, fileSharedPath, fileErr := server.materializeInboundFile(roomID, externalID)
		if fileErr != nil && !(allowMissingMedia && errors.Is(fileErr, errInboundMediaNotReady)) {
			return nil, fileErr
		}
		if fileErr == nil {
			filename = file.Filename
			sharedPath = fileSharedPath
		}
		fileItem := map[string]any{"filename": filename}
		if sharedPath != "" {
			fileItem["shared_path"] = sharedPath
		}
		items = []map[string]any{{
			"type": fileItemType, "create_time_ms": createdAt, "is_completed": true,
			"msg_id": externalID, "file_item": fileItem,
		}}
	}
	view := map[string]any{
		"seq": integerOr(message["local_id"], messageID), "message_id": messageID,
		"msgid": externalID, "client_id": externalID,
		"from_user_id": senderID, "to_user_id": accountID, "ilink_user_id": senderID,
		"create_time_ms": createdAt, "update_time_ms": createdAt, "session_id": roomID,
		"message_type": 1, "message_state": 2,
		"context_token": server.contextToken(roomID), "text": text,
		"item_list":      items,
		"wechat_msgtype": messageType(message),
	}
	if sharedPath != "" {
		view["shared_path"] = sharedPath
	}
	if kind == "file" {
		body, _ := items[0]["file_item"].(map[string]any)
		view["filename"] = stringValue(body["filename"])
	}
	if name := strings.TrimSpace(stringValue(message["conversation_name"])); name != "" {
		view["conversation_name"] = name
	}
	if remark := strings.TrimSpace(stringValue(message["conversation_remark"])); remark != "" {
		view["conversation_remark"] = remark
	}
	if strings.HasSuffix(roomID, "@chatroom") {
		view["group_id"] = roomID
		view["mentioned_me"] = containsAccountID(message["at_user_ids"], accountID)
	}
	return view, nil
}

func containsAccountID(value any, accountID string) bool {
	userIDs, _ := value.([]string)
	for _, userID := range userIDs {
		if userID == accountID {
			return true
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
	items, err := outboundItems(body.Message)
	if errors.Is(err, errUnsupportedOutboundItem) {
		writeError(response, http.StatusNotImplemented, err.Error())
		return
	}
	if err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	target, err := server.outboundTarget(body.Message.ContextToken)
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
	if body.Message.ClientID != "" {
		resultID = body.Message.ClientID
	}
	server.logger.Info("WeChat message sent", "target", target, "client_msg_id", resultID)
	writeJSON(response, http.StatusOK, sendSuccess(resultID))
}

func (server *Server) getConfig(response http.ResponseWriter, request *http.Request) {
	if !server.authenticate(response, request) {
		return
	}
	var body struct {
		UserID       string `json:"ilink_user_id"`
		ContextToken string `json:"context_token"`
	}
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	userID := strings.TrimSpace(body.UserID)
	if userID == "" && strings.TrimSpace(body.ContextToken) != "" {
		var context contextToken
		if err := signedpayload.Decode(server.apiToken, body.ContextToken, &context); err != nil {
			writeError(response, http.StatusBadRequest, "invalid context_token: "+err.Error())
			return
		}
		userID = strings.TrimSpace(context.Target)
	}
	if userID == "" {
		writeError(response, http.StatusBadRequest, "ilink_user_id or context_token is required")
		return
	}
	ticket, _ := signedpayload.Encode(server.apiToken, typingTicket{UserID: userID})
	writeJSON(response, http.StatusOK, map[string]any{"ret": 0, "typing_ticket": ticket})
}

func (server *Server) sendTyping(response http.ResponseWriter, request *http.Request) {
	if !server.authenticate(response, request) {
		return
	}
	var body struct {
		UserID string `json:"ilink_user_id"`
		Ticket string `json:"typing_ticket"`
		Status *int   `json:"status"`
	}
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	if body.Status == nil || (*body.Status != 1 && *body.Status != 2) {
		writeError(response, http.StatusBadRequest, "status must be 1 or 2")
		return
	}
	var ticket typingTicket
	if err := signedpayload.Decode(server.apiToken, body.Ticket, &ticket); err != nil {
		writeError(response, http.StatusBadRequest, "invalid typing_ticket: "+err.Error())
		return
	}
	if strings.TrimSpace(body.UserID) != "" && body.UserID != ticket.UserID {
		writeError(response, http.StatusBadRequest, "typing_ticket user mismatch")
		return
	}
	writeError(response, http.StatusNotImplemented, "WeChat Linux UI does not expose a reliable typing indicator action")
}

func (server *Server) notifyConnection(response http.ResponseWriter, request *http.Request) {
	if !server.authenticate(response, request) {
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"ret": 0})
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

func (server *Server) outboundTarget(rawToken string) (string, error) {
	if strings.TrimSpace(rawToken) == "" {
		return "", errors.New("msg.context_token is required")
	}
	var context contextToken
	if err := signedpayload.Decode(server.apiToken, rawToken, &context); err != nil {
		return "", fmt.Errorf("invalid context_token: %w", err)
	}
	if strings.TrimSpace(context.Target) == "" {
		return "", errors.New("msg.context_token has no outbound target")
	}
	return context.Target, nil
}

func (server *Server) contextToken(target string) string {
	token, _ := signedpayload.Encode(server.apiToken, contextToken{Target: target})
	return token
}

func (server *Server) baseURL(request *http.Request) string {
	if server.publicBaseURL != "" {
		return server.publicBaseURL
	}
	return "http://" + request.Host
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

func containsToken(tokens []string, expected string) bool {
	for _, token := range tokens {
		if strings.TrimSpace(token) == expected {
			return true
		}
	}
	return false
}

var errUnsupportedOutboundItem = errors.New("only text, image, and file sending are supported")

func outboundItems(message outboundMessage) ([]sender.Item, error) {
	items := make([]sender.Item, 0, len(message.Items))
	for _, item := range message.Items {
		switch integerValue(item["type"]) {
		case textItemType:
			body, ok := item["text_item"].(map[string]any)
			if !ok {
				return nil, errors.New("text_item must be an object")
			}
			text := strings.TrimSpace(stringValue(body["text"]))
			if text == "" {
				return nil, errors.New("text_item.text is required")
			}
			items = append(items, sender.Item{Kind: "text", Text: text})
		case imageItemType:
			sharedPath, err := outboundImagePath(item)
			if err != nil {
				return nil, err
			}
			items = append(items, sender.Item{Kind: "image", SharedPath: sharedPath})
		case fileItemType:
			sharedPath, err := outboundFilePath(item)
			if err != nil {
				return nil, err
			}
			items = append(items, sender.Item{Kind: "file", SharedPath: sharedPath})
		default:
			return nil, errUnsupportedOutboundItem
		}
	}
	if len(items) == 0 {
		return nil, errors.New("msg.item_list is required")
	}
	return items, nil
}

func messageType(message map[string]any) string {
	if value := stringValue(message["msgtype"]); value != "" {
		return value
	}
	return "text"
}

func messageDisplayText(message map[string]any) string {
	kind := messageType(message)
	if body, ok := message[kind].(map[string]any); ok {
		if content := strings.TrimSpace(stringValue(body["content"])); content != "" {
			return content
		}
	}
	switch kind {
	case "image":
		return "[图片]"
	case "voice":
		return "[语音]"
	case "video":
		return "[视频]"
	case "emotion":
		return "[表情]"
	case "location":
		return "[位置]"
	case "voip":
		return "[通话]"
	case "system":
		return "[系统消息]"
	case "revoke":
		return "[撤回了一条消息]"
	case "link":
		return compoundDisplay("[链接]", message[kind], "title", "description", "link_url", "url")
	case "sphfeed":
		return compoundDisplay("[视频号]", message[kind], "sph_name", "feed_desc", "url")
	default:
		return "[" + kind + "]"
	}
}

func compoundDisplay(prefix string, raw any, keys ...string) string {
	body, _ := raw.(map[string]any)
	parts := []string{}
	seen := map[string]bool{}
	for _, key := range keys {
		value := strings.TrimSpace(stringValue(body[key]))
		if value != "" && !seen[value] {
			parts = append(parts, value)
			seen[value] = true
		}
	}
	if len(parts) == 0 {
		return prefix
	}
	return prefix + " " + strings.Join(parts, "\n")
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func integerValue(value any) int64 {
	switch value := value.(type) {
	case int:
		return int64(value)
	case int64:
		return value
	case float64:
		return int64(value)
	case json.Number:
		result, _ := value.Int64()
		return result
	default:
		return 0
	}
}

func integerOr(value any, fallback int64) int64 {
	if result := integerValue(value); result != 0 {
		return result
	}
	return fallback
}

func stablePositiveID(value string) int64 {
	digest := sha256.Sum256([]byte(value))
	result := int64(0)
	for _, item := range digest[:8] {
		result = result<<8 | int64(item)
	}
	return result & int64(^uint64(0)>>1)
}

func randomID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err == nil {
		return hex.EncodeToString(value)
	}
	return fmt.Sprintf("%032x", time.Now().UnixNano())
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

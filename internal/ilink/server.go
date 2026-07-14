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
	"github.com/netcat-ai/webox/internal/signedpayload"
	"github.com/netcat-ai/webox/internal/wechat"
)

const (
	textItemType       = 1
	getUpdatesTimeout  = 35 * time.Second
	getUpdatesInterval = time.Second
	qrAcquireTimeout   = 20 * time.Second
	qrSessionTTL       = 5 * time.Minute
	maxSendReceipts    = 1024
)

type messageSource interface {
	IsInitialized() bool
	RefreshLoginQRCode() (bool, error)
	ValidatePollCursor(string) error
	PollMessages(string, int) (wechat.PollResult, error)
}

type textSender interface {
	SendText(context.Context, string, string) (sender.Receipt, error)
}

type qrSource interface {
	Latest() (*qrsource.LoginCode, error)
}

type Server struct {
	apiToken          string
	providerAccountID string
	publicBaseURL     string
	messages          messageSource
	sender            textSender
	qr                qrSource
	logger            *slog.Logger

	loginMu sync.Mutex
	login   loginSession
	sendMu  sync.Mutex
	cache   sendReceiptCache

	pollTimeout  time.Duration
	pollInterval time.Duration
	qrTimeout    time.Duration
}

type loginSession struct {
	activeQrcode    string
	activeIssuedAt  time.Time
	confirmedQrcode string
}

type cachedSend struct {
	fingerprint     string
	clientMessageID string
}

type sendReceiptCache struct {
	entries map[string]cachedSend
	order   []string
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
	Text         string           `json:"text"`
	Items        []map[string]any `json:"item_list"`
}

type contextToken struct {
	Target string `json:"target"`
}

type typingTicket struct {
	UserID string `json:"ilink_user_id"`
}

func New(apiToken, providerAccountID, publicBaseURL string, messages messageSource, sender textSender, qr qrSource, logger *slog.Logger) *Server {
	return &Server{
		apiToken: apiToken, providerAccountID: providerAccountID,
		publicBaseURL: strings.TrimRight(strings.TrimSpace(publicBaseURL), "/"),
		messages:      messages, sender: sender, qr: qr, logger: logger,
		cache:       sendReceiptCache{entries: make(map[string]cachedSend)},
		pollTimeout: getUpdatesTimeout, pollInterval: getUpdatesInterval, qrTimeout: qrAcquireTimeout,
	}
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("GET /ilink/bot/get_bot_qrcode", server.getBotQrcode)
	mux.HandleFunc("POST /ilink/bot/get_bot_qrcode", server.getBotQrcode)
	mux.HandleFunc("GET /ilink/bot/get_qrcode_status", server.getQrcodeStatus)
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
		result["bot_token"] = server.apiToken
		result["ilink_bot_id"] = server.providerAccountID
		result["ilink_user_id"] = server.providerAccountID
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
		cursor = result.Cursor
		if len(result.Messages) != 0 || !time.Now().Before(deadline) {
			messages := make([]map[string]any, 0, len(result.Messages))
			for _, message := range result.Messages {
				messages = append(messages, server.standardMessage(message))
			}
			writeJSON(response, http.StatusOK, map[string]any{
				"ret": 0, "msgs": messages, "get_updates_buf": cursor,
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

func (server *Server) standardMessage(message map[string]any) map[string]any {
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
	view := map[string]any{
		"seq": integerOr(message["local_id"], messageID), "message_id": messageID,
		"msgid": externalID, "client_id": externalID,
		"from_user_id": senderID, "to_user_id": server.providerAccountID, "ilink_user_id": senderID,
		"create_time_ms": createdAt, "update_time_ms": createdAt, "session_id": roomID,
		"message_type": 1, "message_state": 2,
		"context_token": server.contextToken(roomID), "text": text,
		"item_list": []map[string]any{{
			"type": textItemType, "create_time_ms": createdAt, "is_completed": true,
			"msg_id": externalID, "text_item": map[string]any{"text": text},
		}},
		"wechat_msgtype": messageType(message),
	}
	if strings.HasSuffix(roomID, "@chatroom") {
		view["group_id"] = roomID
	}
	return view
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
	for _, item := range body.Message.Items {
		for _, key := range []string{"image_item", "voice_item", "file_item", "video_item"} {
			if _, found := item[key]; found {
				writeError(response, http.StatusNotImplemented, "binary media sending is not supported; send an external URL as text")
				return
			}
		}
	}
	clientID := strings.TrimSpace(body.Message.ClientID)
	if len(clientID) > 128 {
		writeError(response, http.StatusBadRequest, "msg.client_id is too long")
		return
	}
	target, err := server.outboundTarget(body.Message.ContextToken)
	if err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	text := outboundText(body.Message)
	if text == "" {
		writeError(response, http.StatusBadRequest, "msg.text or text item is required")
		return
	}
	fingerprint := messageFingerprint(body.Message)

	server.sendMu.Lock()
	defer server.sendMu.Unlock()
	if clientID != "" {
		if cached, found := server.cache.entries[clientID]; found {
			if cached.fingerprint != fingerprint {
				writeError(response, http.StatusBadRequest, "msg.client_id was already used for different content")
				return
			}
			writeJSON(response, http.StatusOK, sendSuccess(cached.clientMessageID))
			return
		}
	}
	receipt, err := server.sender.SendText(request.Context(), target, text)
	if err != nil {
		server.logger.Error("could not send WeChat text", "target", target, "error", err)
		writeError(response, http.StatusInternalServerError, err.Error())
		return
	}
	resultID := receipt.ClientMessageID
	if clientID != "" {
		resultID = clientID
		server.rememberSend(clientID, fingerprint, resultID)
	}
	server.logger.Info("WeChat text sent", "target", target, "client_msg_id", resultID)
	writeJSON(response, http.StatusOK, sendSuccess(resultID))
}

func (server *Server) rememberSend(clientID, fingerprint, messageID string) {
	if _, exists := server.cache.entries[clientID]; !exists {
		for len(server.cache.entries) >= maxSendReceipts {
			oldest := server.cache.order[0]
			server.cache.order = server.cache.order[1:]
			delete(server.cache.entries, oldest)
		}
		server.cache.order = append(server.cache.order, clientID)
	}
	server.cache.entries[clientID] = cachedSend{fingerprint: fingerprint, clientMessageID: messageID}
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

func outboundText(message outboundMessage) string {
	if text := strings.TrimSpace(message.Text); text != "" {
		return text
	}
	for _, item := range message.Items {
		if text := strings.TrimSpace(stringValue(item["text"])); text != "" {
			return text
		}
		if textItem, ok := item["text_item"].(map[string]any); ok {
			if text := strings.TrimSpace(stringValue(textItem["text"])); text != "" {
				return text
			}
		}
	}
	return ""
}

func messageFingerprint(message outboundMessage) string {
	data, _ := json.Marshal(message)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
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

package wecom

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/netcat-ai/webox/internal/sender"
	"github.com/netcat-ai/webox/internal/wechat"
)

const (
	subscribe       = "aibot_subscribe"
	messageCallback = "aibot_msg_callback"
	respondMessage  = "aibot_respond_msg"
	sendMessage     = "aibot_send_msg"
	pingCommand     = "ping"
	pollInterval    = time.Second
	authTimeout     = 10 * time.Second
	replyContextTTL = 10 * time.Minute
)

type Server struct {
	botID     string
	botSecret string
	wechat    *wechat.State
	sendQueue chan<- sender.Job
	logger    *slog.Logger

	activeConnection atomic.Uint64
	cursorMu         sync.Mutex
	pollCursor       string
	upgrader         websocket.Upgrader
}

type wireFrame struct {
	Command string          `json:"cmd"`
	Headers wireHeaders     `json:"headers"`
	Body    json.RawMessage `json:"body"`
}

type wireHeaders struct {
	RequestID string `json:"req_id"`
}

type subscribeBody struct {
	BotID  string `json:"bot_id"`
	Secret string `json:"secret"`
}

type replyContext struct {
	target    string
	createdAt time.Time
}

type incomingMessage struct {
	kind int
	data []byte
	err  error
}

func New(botID, botSecret string, state *wechat.State, sendQueue chan<- sender.Job, logger *slog.Logger) *Server {
	return &Server{
		botID: botID, botSecret: botSecret, wechat: state, sendQueue: sendQueue, logger: logger,
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
	}
}

func (server *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	connection, err := server.upgrader.Upgrade(response, request, nil)
	if err != nil {
		server.logger.Warn("could not upgrade wecom websocket", "error", err)
		return
	}
	defer func() { _ = connection.Close() }()
	server.run(connection)
}

func (server *Server) run(connection *websocket.Conn) {
	connectionID := server.activeConnection.Add(1)
	if err := server.authenticate(connection); err != nil {
		return
	}
	server.logger.Info("wecom websocket connected", "connection_id", connectionID, "bot_id", server.botID)
	defer server.logger.Info("wecom websocket disconnected", "connection_id", connectionID, "bot_id", server.botID)

	incoming := make(chan incomingMessage)
	done := make(chan struct{})
	defer close(done)
	go readMessages(connection, incoming, done)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	replies := make(map[string]replyContext)
	streams := make(map[string]string)

	if !server.pushWechatMessages(connection, replies) {
		return
	}
	for server.activeConnection.Load() == connectionID {
		select {
		case message := <-incoming:
			if message.err != nil || !server.handleMessage(connection, message, replies, streams) {
				return
			}
		case now := <-ticker.C:
			for requestID, context := range replies {
				if now.Sub(context.createdAt) >= replyContextTTL {
					delete(replies, requestID)
					for key := range streams {
						if strings.HasPrefix(key, requestID+"\x00") {
							delete(streams, key)
						}
					}
				}
			}
			if !server.pushWechatMessages(connection, replies) {
				return
			}
		}
	}
}

func (server *Server) authenticate(connection *websocket.Conn) error {
	if err := connection.SetReadDeadline(time.Now().Add(authTimeout)); err != nil {
		return err
	}
	kind, data, err := connection.ReadMessage()
	if err != nil {
		return err
	}
	_ = connection.SetReadDeadline(time.Time{})
	if kind != websocket.TextMessage {
		return errors.New("subscribe required")
	}
	frame, err := parseFrame(data)
	if err != nil {
		return err
	}
	if frame.Command != subscribe {
		_ = writeAck(connection, frame.Headers.RequestID, 400, "subscribe required")
		return errors.New("subscribe required")
	}
	var body subscribeBody
	if json.Unmarshal(frame.Body, &body) != nil {
		return errors.New("invalid subscribe body")
	}
	if body.BotID != server.botID || body.Secret != server.botSecret {
		_ = writeAck(connection, frame.Headers.RequestID, 401, "authentication failed")
		return errors.New("authentication failed")
	}
	return writeAck(connection, frame.Headers.RequestID, 0, "ok")
}

func readMessages(connection *websocket.Conn, target chan<- incomingMessage, done <-chan struct{}) {
	defer close(target)
	for {
		kind, data, err := connection.ReadMessage()
		select {
		case target <- incomingMessage{kind: kind, data: data, err: err}:
		case <-done:
			return
		}
		if err != nil {
			return
		}
	}
}

func (server *Server) handleMessage(connection *websocket.Conn, incoming incomingMessage, replies map[string]replyContext, streams map[string]string) bool {
	if incoming.kind == websocket.CloseMessage {
		return false
	}
	if incoming.kind != websocket.TextMessage {
		return true
	}
	frame, err := parseFrame(incoming.data)
	if err != nil {
		return false
	}
	switch frame.Command {
	case pingCommand:
	case respondMessage:
		err = server.handleReply(frame, replies, streams)
	case sendMessage:
		err = server.handleProactiveMessage(frame)
	default:
		err = fmt.Errorf("unsupported command: %s", frame.Command)
	}
	if err == nil {
		return writeAck(connection, frame.Headers.RequestID, 0, "ok") == nil
	}
	server.logger.Warn("wecom command failed", "cmd", frame.Command, "error", err)
	return writeAck(connection, frame.Headers.RequestID, 500, "command failed") == nil
}

func (server *Server) handleReply(frame wireFrame, replies map[string]replyContext, streams map[string]string) error {
	var body struct {
		Stream *struct {
			ID      string `json:"id"`
			Content string `json:"content"`
			Finish  bool   `json:"finish"`
		} `json:"stream"`
	}
	if json.Unmarshal(frame.Body, &body) != nil || body.Stream == nil {
		return errors.New("stream body is required")
	}
	streamID := strings.TrimSpace(body.Stream.ID)
	if streamID == "" {
		streamID = frame.Headers.RequestID
	}
	key := frame.Headers.RequestID + "\x00" + streamID
	content := strings.TrimSpace(body.Stream.Content)
	if content != "" {
		// WeCom sends the accumulated document, so replacing avoids duplicated prefixes.
		streams[key] = content
	}
	if !body.Stream.Finish {
		return nil
	}
	context, found := replies[frame.Headers.RequestID]
	if !found {
		return errors.New("reply context is missing or expired")
	}
	finalContent := streams[key]
	if finalContent == "" {
		return errors.New("final reply is empty")
	}
	if err := server.enqueue(context.target, finalContent); err != nil {
		return err
	}
	delete(streams, key)
	delete(replies, frame.Headers.RequestID)
	return nil
}

func (server *Server) handleProactiveMessage(frame wireFrame) error {
	var body map[string]any
	if json.Unmarshal(frame.Body, &body) != nil {
		return errors.New("invalid message body")
	}
	target := stringValue(body["chatid"])
	if target == "" {
		return errors.New("chatid is required")
	}
	content := messageContent(body)
	if content == "" {
		return errors.New("text is required")
	}
	return server.enqueue(target, content)
}

func (server *Server) enqueue(target, content string) error {
	select {
	case server.sendQueue <- sender.Job{Target: target, Content: content}:
		return nil
	default:
		return errors.New("send queue rejected message")
	}
}

func (server *Server) pushWechatMessages(connection *websocket.Conn, replies map[string]replyContext) bool {
	if !server.wechat.IsInitialized() {
		return true
	}
	server.cursorMu.Lock()
	cursor := server.pollCursor
	server.cursorMu.Unlock()
	result, err := server.wechat.PollMessages(cursor, 100)
	if err != nil {
		server.logger.Warn("could not poll WeChat messages", "error", err)
		return true
	}
	for _, message := range result.Messages {
		body, target, ok := callbackBody(server.botID, message)
		if !ok {
			continue
		}
		requestID := randomID()
		replies[requestID] = replyContext{target: target, createdAt: time.Now()}
		if err := connection.WriteJSON(map[string]any{
			"cmd": messageCallback, "headers": map[string]any{"req_id": requestID}, "body": body,
		}); err != nil {
			return false
		}
	}
	// Advancing after writes gives reconnects at-least-once delivery instead of losing a frame.
	server.cursorMu.Lock()
	server.pollCursor = result.Cursor
	server.cursorMu.Unlock()
	return true
}

func callbackBody(botID string, message map[string]any) (map[string]any, string, bool) {
	if stringValue(message["msgtype"]) != "text" {
		return nil, "", false
	}
	messageID := stringValue(message["msgid"])
	target := stringValue(message["roomid"])
	senderID := stringValue(message["from"])
	text, _ := message["text"].(map[string]any)
	content := stringValue(text["content"])
	if messageID == "" || target == "" || senderID == "" {
		return nil, "", false
	}
	group := strings.HasSuffix(target, "@chatroom")
	body := map[string]any{
		"msgid": messageID, "aibotid": botID,
		"chattype":    map[bool]string{true: "group", false: "single"}[group],
		"from":        map[string]any{"userid": senderID},
		"create_time": integerValue(message["msgtime"]) / 1000,
		"msgtype":     "text", "text": map[string]any{"content": content},
	}
	if group {
		body["chatid"] = target
	}
	return body, target, true
}

func messageContent(body map[string]any) string {
	for _, kind := range []string{"text", "markdown"} {
		part, _ := body[kind].(map[string]any)
		if content := stringValue(part["content"]); content != "" {
			return content
		}
	}
	return ""
}

func parseFrame(data []byte) (wireFrame, error) {
	var frame wireFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return wireFrame{}, err
	}
	if frame.Headers.RequestID == "" {
		return wireFrame{}, errors.New("req_id is required")
	}
	if len(frame.Body) == 0 {
		frame.Body = json.RawMessage("{}")
	}
	return frame, nil
}

func writeAck(connection *websocket.Conn, requestID string, code int, message string) error {
	return connection.WriteJSON(map[string]any{
		"headers": map[string]any{"req_id": requestID}, "errcode": code, "errmsg": message,
	})
}

func stringValue(value any) string {
	result, _ := value.(string)
	return strings.TrimSpace(result)
}

func integerValue(value any) int64 {
	switch value := value.(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	case json.Number:
		result, _ := value.Int64()
		return result
	default:
		return 0
	}
}

func randomID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err == nil {
		return hex.EncodeToString(value)
	}
	return fmt.Sprintf("%032x", time.Now().UnixNano())
}

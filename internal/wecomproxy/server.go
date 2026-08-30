package wecomproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/netcat-ai/webox/internal/wecomarchive"
)

const (
	defaultPollTimeout  = 35 * time.Second
	defaultPollInterval = time.Second
	getUpdatesLimit     = 100
)

type archiveSource interface {
	Identity() wecomarchive.Identity
	GetUpdates(context.Context, uint64, int) ([]json.RawMessage, uint64, error)
}

type Server struct {
	apiToken    string
	source      archiveSource
	logger      *slog.Logger
	pollTimeout time.Duration
	pollEvery   time.Duration
}

type getUpdatesRequest struct {
	Cursor string `json:"cursor"`
}

func New(apiToken string, source archiveSource, logger *slog.Logger) *Server {
	return &Server{
		apiToken: apiToken, source: source, logger: logger,
		pollTimeout: defaultPollTimeout, pollEvery: defaultPollInterval,
	}
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("GET /ilink/bot/userinfo", server.userInfo)
	mux.HandleFunc("GET /ilink/bot/contacts", server.contacts)
	mux.HandleFunc("POST /ilink/bot/getupdates", server.getUpdates)
	mux.HandleFunc("POST /ilink/bot/sendmessage", server.sendMessage)
	return mux
}

func (server *Server) health(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]any{"ok": true, "ready": true})
}

func (server *Server) userInfo(response http.ResponseWriter, request *http.Request) {
	if !server.authenticate(response, request) {
		return
	}
	identity := server.source.Identity()
	result := map[string]any{
		"ret": 0, "account_id": identity.AccountID, "wechat_id": identity.WeChatID,
	}
	if nickname := strings.TrimSpace(identity.Nickname); nickname != "" {
		result["nickname"] = nickname
	}
	writeJSON(response, http.StatusOK, result)
}

func (server *Server) contacts(response http.ResponseWriter, request *http.Request) {
	if !server.authenticate(response, request) {
		return
	}
	writeError(response, http.StatusNotImplemented, "contacts are not supported by the WeCom archive proxy")
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
	var cursor uint64
	if body.Cursor != "" {
		value, err := strconv.ParseUint(body.Cursor, 10, 64)
		if err != nil {
			writeError(response, http.StatusBadRequest, "cursor must be an empty or non-negative decimal Finance seq")
			return
		}
		cursor = value
	}
	deadline := time.Now().Add(server.pollTimeout)
	for {
		messages, next, err := server.source.GetUpdates(request.Context(), cursor, getUpdatesLimit)
		if err != nil {
			server.logger.Warn("WeCom archive polling failed", "error", err)
			writeJSON(response, http.StatusOK, map[string]any{
				"ret": -14, "errcode": -14, "errmsg": "WeCom archive is unavailable", "messages": []any{}, "cursor": body.Cursor,
			})
			return
		}
		if messages == nil {
			messages = make([]json.RawMessage, 0)
		}
		if len(messages) != 0 || next != cursor || !time.Now().Before(deadline) {
			writeJSON(response, http.StatusOK, map[string]any{
				"ret": 0, "messages": messages, "cursor": strconv.FormatUint(next, 10),
				"longpolling_timeout_ms": server.pollTimeout.Milliseconds(),
			})
			return
		}
		if !wait(request.Context(), server.pollEvery) {
			return
		}
	}
}

func (server *Server) sendMessage(response http.ResponseWriter, request *http.Request) {
	if !server.authenticate(response, request) {
		return
	}
	writeError(response, http.StatusNotImplemented, "WeCom archive does not provide message sending")
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
	authorization := request.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer ")) != server.apiToken {
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

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

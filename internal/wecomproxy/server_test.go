package wecomproxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/netcat-ai/webox/internal/wecomarchive"
)

type fakeSource struct {
	updates []json.RawMessage
	next    uint64
}

func (source fakeSource) Identity() wecomarchive.Identity {
	return wecomarchive.Identity{AccountID: "member", WeChatID: "archive", Nickname: "归档"}
}

func (source fakeSource) GetUpdates(context.Context, uint64, int) ([]json.RawMessage, uint64, error) {
	return source.updates, source.next, nil
}

func TestGetUpdatesPreservesUnknownMessageBody(t *testing.T) {
	server := New("token", fakeSource{updates: []json.RawMessage{json.RawMessage(`{"msgid":"one","roomid":"room","msgtype":"emotion","emotion":{"type":1}}`)}, next: 7}, discardLogger())
	request := httptest.NewRequest(http.MethodPost, "/ilink/bot/getupdates", strings.NewReader(`{"cursor":"6"}`))
	authorize(request)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"emotion":{"type":1}`) || !strings.Contains(response.Body.String(), `"cursor":"7"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSendMessageIsExplicitlyUnsupported(t *testing.T) {
	server := New("token", fakeSource{}, discardLogger())
	request := httptest.NewRequest(http.MethodPost, "/ilink/bot/sendmessage", strings.NewReader(`{"msgs":[]}`))
	authorize(request)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEmptyPollReturnsAfterConfiguredTimeout(t *testing.T) {
	server := New("token", fakeSource{}, discardLogger())
	server.pollTimeout = time.Millisecond
	server.pollEvery = time.Millisecond
	request := httptest.NewRequest(http.MethodPost, "/ilink/bot/getupdates", strings.NewReader(`{"cursor":""}`))
	authorize(request)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"messages":[]`) || !strings.Contains(response.Body.String(), `"cursor":"0"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func authorize(request *http.Request) {
	request.Header.Set("AuthorizationType", "ilink_bot_token")
	request.Header.Set("X-WECHAT-UIN", "archive")
	request.Header.Set("Authorization", "Bearer token")
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

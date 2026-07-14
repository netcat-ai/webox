package ilink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/netcat-ai/webox/internal/qrsource"
	"github.com/netcat-ai/webox/internal/sender"
	"github.com/netcat-ai/webox/internal/signedpayload"
	"github.com/netcat-ai/webox/internal/wechat"
)

type fakeMessages struct {
	initialized bool
	validateErr error
	pollResult  wechat.PollResult
	pollErr     error
	refreshes   int
}

func (source *fakeMessages) IsInitialized() bool { return source.initialized }
func (source *fakeMessages) RefreshLoginQRCode() (bool, error) {
	source.refreshes++
	return true, nil
}
func (source *fakeMessages) ValidatePollCursor(string) error { return source.validateErr }
func (source *fakeMessages) PollMessages(string, int) (wechat.PollResult, error) {
	return source.pollResult, source.pollErr
}

type fakeSender struct {
	calls  int
	target string
	text   string
	err    error
}

func (service *fakeSender) SendText(_ context.Context, target, text string) (sender.Receipt, error) {
	service.calls++
	service.target, service.text = target, text
	return sender.Receipt{ClientMessageID: "ui-message"}, service.err
}

type fakeQR struct {
	code *qrsource.LoginCode
	err  error
}

func (source *fakeQR) Latest() (*qrsource.LoginCode, error) { return source.code, source.err }

func TestWeComRouteIsRemoved(t *testing.T) {
	server, _, _, _ := testServer()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/wecom", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestHealthOnlyExposesReadiness(t *testing.T) {
	server, _, _, _ := testServer()
	response := perform(server, http.MethodGet, "/healthz", nil, false)
	body := responseJSON(t, response)
	if response.Code != http.StatusOK || body["ok"] != true || body["ready"] != false || len(body) != 2 {
		t.Fatalf("status=%d body=%#v", response.Code, body)
	}
}

func TestQRCodeLoginReturnsRealCodeAndConfirmsIssuedSession(t *testing.T) {
	server, messages, _, qr := testServer()
	qr.code = &qrsource.LoginCode{ID: "xvfb-qr-current", LoginURL: "https://weixin.qq.com/x/test"}

	issued := perform(server, http.MethodPost, "/ilink/bot/get_bot_qrcode?bot_type=3", map[string]any{"local_token_list": []string{}}, false)
	issuedBody := responseJSON(t, issued)
	if issued.Code != http.StatusOK || issuedBody["qrcode"] != "xvfb-qr-current" || issuedBody["qrcode_img_content"] != qr.code.LoginURL {
		t.Fatalf("status=%d body=%#v", issued.Code, issuedBody)
	}

	waiting := perform(server, http.MethodGet, "/ilink/bot/get_qrcode_status?qrcode=xvfb-qr-current", nil, false)
	if body := responseJSON(t, waiting); body["status"] != "wait" {
		t.Fatalf("waiting body=%#v", body)
	}

	messages.initialized = true
	qr.code = nil
	confirmed := perform(server, http.MethodGet, "/ilink/bot/get_qrcode_status?qrcode=xvfb-qr-current", nil, false)
	body := responseJSON(t, confirmed)
	if body["status"] != "confirmed" || body["bot_token"] != "api-token" || body["ilink_bot_id"] != "webox-account" || body["baseurl"] != "http://example.test" {
		t.Fatalf("confirmed body=%#v", body)
	}
}

func TestLoggedInResumeRequiresMatchingLocalToken(t *testing.T) {
	server, messages, _, _ := testServer()
	messages.initialized = true

	denied := perform(server, http.MethodPost, "/ilink/bot/get_bot_qrcode?bot_type=3", map[string]any{"local_token_list": []string{"wrong"}}, false)
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", denied.Code, denied.Body.String())
	}
	accepted := perform(server, http.MethodPost, "/ilink/bot/get_bot_qrcode?bot_type=3", map[string]any{"local_token_list": []string{"api-token"}}, false)
	body := responseJSON(t, accepted)
	if accepted.Code != http.StatusOK || body["qrcode_img_content"] != "" {
		t.Fatalf("status=%d body=%#v", accepted.Code, body)
	}
}

func TestExpiredQRCodeIsRefreshedBeforeReissue(t *testing.T) {
	server, messages, _, qr := testServer()
	server.login.register("expired-code")
	server.login.activeIssuedAt = time.Now().Add(-qrSessionTTL)
	qr.code = &qrsource.LoginCode{ID: "fresh-code", LoginURL: "https://weixin.qq.com/x/fresh"}

	response := perform(server, http.MethodPost, "/ilink/bot/get_bot_qrcode?bot_type=3", map[string]any{"local_token_list": []string{}}, false)
	body := responseJSON(t, response)
	if response.Code != http.StatusOK || body["qrcode"] != "fresh-code" || messages.refreshes != 1 {
		t.Fatalf("status=%d body=%#v refreshes=%d", response.Code, body, messages.refreshes)
	}
}

func TestGetUpdatesMapsMessageAndIssuesReplyContext(t *testing.T) {
	server, messages, _, _ := testServer()
	messages.initialized = true
	messages.pollResult = wechat.PollResult{Cursor: "next-cursor", Messages: []map[string]any{{
		"msgid": "message-1", "local_id": int64(7), "from": "wxid-alice", "roomid": "wxid-alice",
		"msgtime": int64(1781703356000), "msgtype": "text", "text": map[string]any{"content": "hello"},
	}}}

	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": "", "base_info": map[string]any{}}, true)
	body := responseJSON(t, response)
	if response.Code != http.StatusOK || body["ret"] != float64(0) || body["get_updates_buf"] != "next-cursor" {
		t.Fatalf("status=%d body=%#v", response.Code, body)
	}
	messagesView := body["msgs"].([]any)
	message := messagesView[0].(map[string]any)
	if message["from_user_id"] != "wxid-alice" || message["to_user_id"] != "webox-account" || message["text"] != "hello" {
		t.Fatalf("message=%#v", message)
	}
	var context contextToken
	if err := signedpayload.Decode("api-token", message["context_token"].(string), &context); err != nil || context.Target != "wxid-alice" {
		t.Fatalf("context=%#v err=%v", context, err)
	}
}

func TestGetUpdatesRejectsInvalidCursorAndReportsExpiredSession(t *testing.T) {
	server, messages, _, _ := testServer()
	messages.validateErr = errors.New("signature mismatch")
	invalid := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": "tampered"}, true)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", invalid.Code)
	}

	messages.validateErr = nil
	expired := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": "cursor"}, true)
	body := responseJSON(t, expired)
	if expired.Code != http.StatusOK || body["ret"] != float64(-14) || body["get_updates_buf"] != "cursor" {
		t.Fatalf("status=%d body=%#v", expired.Code, body)
	}
}

func TestSendMessageUsesContextAndClientIDIdempotency(t *testing.T) {
	server, messages, outbound, _ := testServer()
	messages.initialized = true
	token := server.contextToken("group@chatroom")
	body := map[string]any{"msg": map[string]any{
		"client_id": "request-1", "context_token": token,
		"item_list": []any{map[string]any{"type": 1, "text_item": map[string]any{"text": "reply"}}},
	}}

	first := perform(server, http.MethodPost, "/ilink/bot/sendmessage", body, true)
	if result := responseJSON(t, first); first.Code != http.StatusOK || result["client_msg_id"] != "request-1" {
		t.Fatalf("status=%d result=%#v", first.Code, result)
	}
	second := perform(server, http.MethodPost, "/ilink/bot/sendmessage", body, true)
	if second.Code != http.StatusOK || outbound.calls != 1 || outbound.target != "group@chatroom" || outbound.text != "reply" {
		t.Fatalf("status=%d sender=%#v", second.Code, outbound)
	}

	body["msg"].(map[string]any)["item_list"] = []any{map[string]any{"type": 1, "text_item": map[string]any{"text": "changed"}}}
	conflict := perform(server, http.MethodPost, "/ilink/bot/sendmessage", body, true)
	if conflict.Code != http.StatusBadRequest || outbound.calls != 1 {
		t.Fatalf("status=%d calls=%d", conflict.Code, outbound.calls)
	}
}

func TestSendMessageRejectsMissingContextAndBinaryMedia(t *testing.T) {
	server, messages, outbound, _ := testServer()
	messages.initialized = true
	missing := perform(server, http.MethodPost, "/ilink/bot/sendmessage", map[string]any{"msg": map[string]any{"text": "hello"}}, true)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", missing.Code)
	}

	media := perform(server, http.MethodPost, "/ilink/bot/sendmessage", map[string]any{"msg": map[string]any{
		"context_token": server.contextToken("wxid-a"),
		"item_list":     []any{map[string]any{"type": 2, "image_item": map[string]any{}}},
	}}, true)
	if media.Code != http.StatusNotImplemented || outbound.calls != 0 {
		t.Fatalf("status=%d calls=%d", media.Code, outbound.calls)
	}
}

func TestBusinessRoutesRequireStandardILinkHeaders(t *testing.T) {
	server, _, _, _ := testServer()
	response := perform(server, http.MethodPost, "/ilink/bot/msg/notifystart", map[string]any{}, false)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestGetConfigIssuesBoundTypingTicket(t *testing.T) {
	server, _, _, _ := testServer()
	response := perform(server, http.MethodPost, "/ilink/bot/getconfig", map[string]any{"ilink_user_id": "wxid-a"}, true)
	body := responseJSON(t, response)
	var ticket typingTicket
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%#v", response.Code, body)
	}
	if err := signedpayload.Decode("api-token", body["typing_ticket"].(string), &ticket); err != nil || ticket.UserID != "wxid-a" {
		t.Fatalf("ticket=%#v err=%v", ticket, err)
	}
}

func TestLoginSessionDoesNotConfirmUnknownOrExpiredCode(t *testing.T) {
	var session loginSession
	session.register("known")
	if got := session.status("unknown", "", true, time.Now()); got != "expired" {
		t.Fatalf("unknown=%q", got)
	}
	if got := session.status("known", "known", false, time.Now().Add(qrSessionTTL)); got != "expired" {
		t.Fatalf("expired=%q", got)
	}
}

func testServer() (*Server, *fakeMessages, *fakeSender, *fakeQR) {
	messages := &fakeMessages{}
	outbound := &fakeSender{}
	qr := &fakeQR{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New("api-token", "webox-account", "", messages, outbound, qr, logger)
	server.pollTimeout = time.Millisecond
	server.pollInterval = time.Millisecond
	server.qrTimeout = time.Millisecond
	return server, messages, outbound, qr
}

func perform(server *Server, method, path string, body any, authenticated bool) *httptest.ResponseRecorder {
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	request := httptest.NewRequest(method, "http://example.test"+path, bytes.NewReader(data))
	if authenticated {
		request.Header.Set("AuthorizationType", "ilink_bot_token")
		request.Header.Set("Authorization", "Bearer api-token")
		request.Header.Set("X-WECHAT-UIN", "MTIz")
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func responseJSON(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
	return body
}

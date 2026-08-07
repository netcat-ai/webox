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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/netcat-ai/webox/internal/qrsource"
	"github.com/netcat-ai/webox/internal/sender"
	"github.com/netcat-ai/webox/internal/sharedmedia"
	"github.com/netcat-ai/webox/internal/signedpayload"
	"github.com/netcat-ai/webox/internal/wechat"
	"github.com/netcat-ai/webox/internal/wechatdb"
)

type fakeMessages struct {
	initialized bool
	accountID   string
	userInfo    wechat.UserInfo
	accountErr  error
	userInfoErr error
	validateErr error
	pollResult  wechat.PollResult
	pollErr     error
	refreshes   int
	media       *wechatdb.MediaFile
	mediaErr    error
	file        *wechatdb.LocalFile
	fileErr     error
}

func (source *fakeMessages) IsInitialized() bool { return source.initialized }
func (source *fakeMessages) AccountID() (string, error) {
	return source.accountID, source.accountErr
}
func (source *fakeMessages) UserInfo() (wechat.UserInfo, error) {
	return source.userInfo, source.userInfoErr
}
func (source *fakeMessages) RefreshLoginQRCode() (bool, error) {
	source.refreshes++
	return true, nil
}
func (source *fakeMessages) ValidatePollCursor(string) error { return source.validateErr }
func (source *fakeMessages) PollMessages(string, int) (wechat.PollResult, error) {
	result := source.pollResult
	if result.AccountID == "" {
		result.AccountID = source.accountID
	}
	return result, source.pollErr
}
func (source *fakeMessages) ReadImage(string, string) (*wechatdb.MediaFile, error) {
	return source.media, source.mediaErr
}
func (source *fakeMessages) ReadFile(string, string) (*wechatdb.LocalFile, error) {
	return source.file, source.fileErr
}

type fakeSender struct {
	calls      int
	imageCalls int
	fileCalls  int
	target     string
	text       string
	imagePath  string
	filePath   string
	items      []sender.Item
	err        error
}

func (service *fakeSender) Send(_ context.Context, target string, items []sender.Item) (sender.Receipt, error) {
	service.calls++
	service.target = target
	service.items = append([]sender.Item(nil), items...)
	for _, item := range items {
		switch item.Kind {
		case "text":
			service.text = item.Text
		case "image":
			service.imageCalls++
			service.imagePath = item.SharedPath
		case "file":
			service.fileCalls++
			service.filePath = item.SharedPath
		}
	}
	return sender.Receipt{ClientMessageID: "ui-message"}, service.err
}

type fakeQR struct {
	code *qrsource.LoginCode
	err  error
}

func (source *fakeQR) Latest() (*qrsource.LoginCode, error) { return source.code, source.err }

func TestWeComRouteIsRemoved(t *testing.T) {
	server, _, _, _ := testServer(t)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/wecom", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestHealthOnlyExposesReadiness(t *testing.T) {
	server, _, _, _ := testServer(t)
	response := perform(server, http.MethodGet, "/healthz", nil, false)
	body := responseJSON(t, response)
	if response.Code != http.StatusOK || body["ok"] != true || body["ready"] != false || len(body) != 2 {
		t.Fatalf("status=%d body=%#v", response.Code, body)
	}
}

func TestQRCodeLoginReturnsRealCodeAndConfirmsIssuedSession(t *testing.T) {
	server, messages, _, qr := testServer(t)
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
	if body["status"] != "confirmed" || body["bot_token"] != "api-token" || body["ilink_bot_id"] != "wxid-self" || body["baseurl"] != "http://example.test" {
		t.Fatalf("confirmed body=%#v", body)
	}
}

func TestLoggedInResumeRequiresMatchingLocalToken(t *testing.T) {
	server, messages, _, _ := testServer(t)
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
	server, messages, _, qr := testServer(t)
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
	server, messages, _, _ := testServer(t)
	messages.initialized = true
	messages.accountID = "wxid-stale"
	messages.pollResult = wechat.PollResult{AccountID: "wxid-self", Cursor: "next-cursor", Messages: []map[string]any{{
		"msgid": "message-1", "local_id": int64(7), "from": "wxid-alice", "roomid": "wxid-alice",
		"msgtime": int64(1781703356000), "msgtype": "text", "text": map[string]any{"content": "hello"},
		"conversation_name": "Alice", "conversation_remark": "webox.alice",
	}}}

	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": "", "base_info": map[string]any{}}, true)
	body := responseJSON(t, response)
	if response.Code != http.StatusOK || body["ret"] != float64(0) || body["get_updates_buf"] != "next-cursor" {
		t.Fatalf("status=%d body=%#v", response.Code, body)
	}
	messagesView := body["msgs"].([]any)
	message := messagesView[0].(map[string]any)
	if message["from_user_id"] != "wxid-alice" || message["to_user_id"] != "wxid-self" || message["text"] != "hello" {
		t.Fatalf("message=%#v", message)
	}
	if message["conversation_name"] != "Alice" || message["conversation_remark"] != "webox.alice" {
		t.Fatalf("conversation metadata=%#v", message)
	}
	var context contextToken
	if err := signedpayload.Decode("api-token", message["context_token"].(string), &context); err != nil || context.Target != "wxid-alice" {
		t.Fatalf("context=%#v err=%v", context, err)
	}
}

func TestGetUpdatesIncludesOrdinaryLinkTitleDescriptionAndURL(t *testing.T) {
	server, messages, _, _ := testServer(t)
	messages.initialized = true
	messages.pollResult = wechat.PollResult{AccountID: "wxid-self", Cursor: "next-cursor", Messages: []map[string]any{{
		"msgid": "101", "local_id": int64(7), "from": "wxid-alice", "roomid": "wxid-alice",
		"msgtime": int64(1781703356000), "msgtype": "link", "link": map[string]any{
			"title": "文章标题", "description": "文章摘要",
			"url": "https://example.com/article?id=1&from=wechat",
		},
	}}}

	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": ""}, true)
	body := responseJSON(t, response)
	message := body["msgs"].([]any)[0].(map[string]any)
	want := "[链接] 文章标题\n文章摘要\nhttps://example.com/article?id=1&from=wechat"
	if response.Code != http.StatusOK || message["wechat_msgtype"] != "link" || message["text"] != want {
		t.Fatalf("status=%d message=%#v want text=%q", response.Code, message, want)
	}
}

func TestGetUpdatesMarksOnlyExplicitGroupAccountMention(t *testing.T) {
	server, messages, _, _ := testServer(t)
	messages.initialized = true
	messages.pollResult = wechat.PollResult{AccountID: "wxid-self", Cursor: "next-cursor", Messages: []map[string]any{
		{
			"msgid": "message-1", "local_id": int64(7), "from": "wxid-alice", "roomid": "group@chatroom",
			"msgtime": int64(1781703356000), "msgtype": "text", "text": map[string]any{"content": "@Self hello"},
			"at_user_ids": []string{"wxid-self", "wxid-other"},
		},
		{
			"msgid": "message-2", "local_id": int64(8), "from": "wxid-alice", "roomid": "group@chatroom",
			"msgtime": int64(1781703357000), "msgtype": "text", "text": map[string]any{"content": "@all hello"},
			"at_user_ids": []string{"notify@all"},
		},
	}}

	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": ""}, true)
	body := responseJSON(t, response)
	views := body["msgs"].([]any)
	first := views[0].(map[string]any)
	second := views[1].(map[string]any)
	if response.Code != http.StatusOK || first["mentioned_me"] != true || second["mentioned_me"] != false {
		t.Fatalf("status=%d messages=%#v", response.Code, views)
	}
}

func TestGetUpdatesRejectsInvalidCursorAndReportsExpiredSession(t *testing.T) {
	server, messages, _, _ := testServer(t)
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

func TestSendMessageUsesContextAndReturnsClientID(t *testing.T) {
	server, messages, outbound, _ := testServer(t)
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
	body["msg"].(map[string]any)["item_list"] = []any{map[string]any{"type": 1, "text_item": map[string]any{"text": "changed"}}}
	second := perform(server, http.MethodPost, "/ilink/bot/sendmessage", body, true)
	if result := responseJSON(t, second); second.Code != http.StatusOK || result["client_msg_id"] != "request-1" || outbound.calls != 2 || outbound.target != "group@chatroom" || outbound.text != "changed" {
		t.Fatalf("status=%d result=%#v sender=%#v", second.Code, result, outbound)
	}
}

func TestSendMessageBatchesTextImageAndFile(t *testing.T) {
	server, messages, outbound, _ := testServer(t)
	messages.initialized = true
	body := map[string]any{"msg": map[string]any{
		"client_id": "batch-1", "context_token": server.contextToken("group@chatroom"),
		"item_list": []any{
			map[string]any{"type": fileItemType, "file_item": map[string]any{"shared_path": "outbox/report.txt"}},
			map[string]any{"type": textItemType, "text_item": map[string]any{"text": "文件已整理完成"}},
			map[string]any{"type": imageItemType, "image_item": map[string]any{"shared_path": "outbox/chart.png"}},
			map[string]any{"type": textItemType, "text_item": map[string]any{"text": "请查收"}},
		},
	}}

	response := perform(server, http.MethodPost, "/ilink/bot/sendmessage", body, true)
	if response.Code != http.StatusOK || outbound.calls != 1 || len(outbound.items) != 4 {
		t.Fatalf("status=%d sender=%#v", response.Code, outbound)
	}
	if outbound.items[0].Kind != "file" || outbound.items[0].SharedPath != "outbox/report.txt" || outbound.items[1].Kind != "text" || outbound.items[1].Text != "文件已整理完成" || outbound.items[2].Kind != "image" || outbound.items[2].SharedPath != "outbox/chart.png" || outbound.items[3].Kind != "text" || outbound.items[3].Text != "请查收" {
		t.Fatalf("items=%#v", outbound.items)
	}
}

func TestSharedImageIsSentForEveryRequest(t *testing.T) {
	server, messages, outbound, _ := testServer(t)
	messages.initialized = true
	body := map[string]any{"msg": map[string]any{
		"client_id": "image-request-1", "context_token": server.contextToken("group@chatroom"),
		"item_list": []any{map[string]any{"type": imageItemType, "image_item": map[string]any{
			"shared_path": "outbox/reply.png",
		}}},
	}}
	first := perform(server, http.MethodPost, "/ilink/bot/sendmessage", body, true)
	second := perform(server, http.MethodPost, "/ilink/bot/sendmessage", body, true)
	if first.Code != http.StatusOK || second.Code != http.StatusOK || outbound.imageCalls != 2 {
		t.Fatalf("first=%d second=%d imageCalls=%d", first.Code, second.Code, outbound.imageCalls)
	}
	if outbound.target != "group@chatroom" || outbound.imagePath != "outbox/reply.png" {
		t.Fatalf("target=%q path=%q", outbound.target, outbound.imagePath)
	}
}

func TestIncomingImageIsWrittenToSharedDirectory(t *testing.T) {
	server, messages, _, _ := testServer(t)
	mediaRoot := t.TempDir()
	mediaStore, err := sharedmedia.New(mediaRoot)
	if err != nil {
		t.Fatal(err)
	}
	server.media = mediaStore
	messages.initialized = true
	imageBytes := append([]byte{0xff, 0xd8, 0xff, 0xe0}, []byte("image-bytes")...)
	messages.media = &wechatdb.MediaFile{Data: imageBytes, ContentType: "image/jpeg", Filename: "message.jpg"}
	messages.pollResult = wechat.PollResult{Cursor: "next", Messages: []map[string]any{{
		"msgid": "123", "local_id": int64(7), "from": "wxid-a", "roomid": "wxid-a",
		"msgtime": int64(1781703356000), "msgtype": "image", "image": map[string]any{"content": "[图片]"},
	}}}
	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": ""}, true)
	body := responseJSON(t, response)
	message := body["msgs"].([]any)[0].(map[string]any)
	item := message["item_list"].([]any)[0].(map[string]any)
	imageItem := item["image_item"].(map[string]any)
	sharedPath := imageItem["shared_path"].(string)
	if message["shared_path"] != sharedPath || imageItem["shared_path"] != sharedPath {
		t.Fatalf("message=%#v", message)
	}
	assertNoMediaAvailabilityFields(t, message, imageItem)
	written, err := os.ReadFile(filepath.Join(mediaRoot, filepath.FromSlash(sharedPath)))
	if err != nil || !bytes.Equal(written, imageBytes) {
		t.Fatalf("path=%q data=%x err=%v", sharedPath, written, err)
	}
}

func TestIncomingMediaWaitsWithoutAdvancingCursor(t *testing.T) {
	server, messages, _, _ := testServer(t)
	messages.initialized = true
	messages.pollResult = wechat.PollResult{Cursor: "next", Messages: []map[string]any{
		{
			"msgid": "image-pending", "local_id": int64(11), "from": "wxid-a", "roomid": "wxid-a",
			"msgtime": time.Now().UnixMilli(), "msgtype": "image", "image": map[string]any{"content": "[图片]"},
		},
		{
			"msgid": "file-pending", "local_id": int64(12), "from": "wxid-a", "roomid": "wxid-a",
			"msgtime": time.Now().UnixMilli(), "msgtype": "file", "file": map[string]any{"filename": "pending.zip"},
		},
	}}
	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": "cursor-original"}, true)
	body := responseJSON(t, response)
	if response.Code != http.StatusOK || body["get_updates_buf"] != "cursor-original" || len(body["msgs"].([]any)) != 0 {
		t.Fatalf("pending status=%d body=%#v", response.Code, body)
	}
}

func TestSharedFileIsSentForEveryRequest(t *testing.T) {
	server, messages, outbound, _ := testServer(t)
	messages.initialized = true
	body := map[string]any{"msg": map[string]any{
		"client_id": "file-request-1", "context_token": server.contextToken("group@chatroom"),
		"item_list": []any{map[string]any{"type": fileItemType, "file_item": map[string]any{
			"shared_path": "outbox/report.pdf",
		}}},
	}}
	first := perform(server, http.MethodPost, "/ilink/bot/sendmessage", body, true)
	second := perform(server, http.MethodPost, "/ilink/bot/sendmessage", body, true)
	if first.Code != http.StatusOK || second.Code != http.StatusOK || outbound.fileCalls != 2 {
		t.Fatalf("first=%d second=%d fileCalls=%d", first.Code, second.Code, outbound.fileCalls)
	}
	if outbound.target != "group@chatroom" || outbound.filePath != "outbox/report.pdf" {
		t.Fatalf("target=%q path=%q", outbound.target, outbound.filePath)
	}
}

func TestIncomingFileIsCopiedToSharedDirectory(t *testing.T) {
	server, messages, _, _ := testServer(t)
	mediaRoot := t.TempDir()
	mediaStore, err := sharedmedia.New(mediaRoot)
	if err != nil {
		t.Fatal(err)
	}
	server.media = mediaStore
	messages.initialized = true
	source := filepath.Join(t.TempDir(), "report.pdf")
	fileBytes := []byte("file-bytes")
	if err := os.WriteFile(source, fileBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	messages.file = &wechatdb.LocalFile{Path: source, Filename: "report.pdf"}
	messages.pollResult = wechat.PollResult{Cursor: "next", Messages: []map[string]any{{
		"msgid": "456", "local_id": int64(8), "from": "wxid-a", "roomid": "wxid-a",
		"msgtime": int64(1781703356000), "msgtype": "file", "file": map[string]any{
			"content": "[文件] report.pdf", "filename": "report.pdf",
		},
	}}}
	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": ""}, true)
	body := responseJSON(t, response)
	message := body["msgs"].([]any)[0].(map[string]any)
	item := message["item_list"].([]any)[0].(map[string]any)
	fileItem := item["file_item"].(map[string]any)
	sharedPath := fileItem["shared_path"].(string)
	if message["shared_path"] != sharedPath || fileItem["filename"] != "report.pdf" || fileItem["shared_path"] != sharedPath {
		t.Fatalf("message=%#v", message)
	}
	assertNoMediaAvailabilityFields(t, message, fileItem)
	written, err := os.ReadFile(filepath.Join(mediaRoot, filepath.FromSlash(sharedPath)))
	if err != nil || !bytes.Equal(written, fileBytes) {
		t.Fatalf("path=%q data=%q err=%v", sharedPath, written, err)
	}
}

func TestIncomingTimedOutMediaIsDeliveredWithoutSharedPathAndAdvancesCursor(t *testing.T) {
	server, messages, _, _ := testServer(t)
	messages.initialized = true
	timedOut := time.Now().Add(-inboundMediaWait).UnixMilli()
	messages.pollResult = wechat.PollResult{Cursor: "next", Messages: []map[string]any{
		{
			"msgid": "image-timeout", "local_id": int64(9), "from": "wxid-a", "roomid": "wxid-a",
			"msgtime": timedOut, "msgtype": "image", "image": map[string]any{"content": "[图片]"},
		},
		{
			"msgid": "file-timeout", "local_id": int64(10), "from": "wxid-a", "roomid": "wxid-a",
			"msgtime": timedOut, "msgtype": "file", "file": map[string]any{"filename": "pending.zip"},
		},
	}}
	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": ""}, true)
	body := responseJSON(t, response)
	views := body["msgs"].([]any)
	if response.Code != http.StatusOK || body["get_updates_buf"] != "next" || len(views) != 2 {
		t.Fatalf("status=%d body=%#v", response.Code, body)
	}
	image := views[0].(map[string]any)
	imageItem := image["item_list"].([]any)[0].(map[string]any)["image_item"].(map[string]any)
	if image["shared_path"] != nil || imageItem["shared_path"] != nil {
		t.Fatalf("image=%#v", image)
	}
	assertNoMediaAvailabilityFields(t, image, imageItem)
	file := views[1].(map[string]any)
	fileItem := file["item_list"].([]any)[0].(map[string]any)["file_item"].(map[string]any)
	if file["filename"] != "pending.zip" || file["shared_path"] != nil || fileItem["shared_path"] != nil {
		t.Fatalf("file=%#v", file)
	}
	assertNoMediaAvailabilityFields(t, file, fileItem)
}

func assertNoMediaAvailabilityFields(t *testing.T, message, item map[string]any) {
	t.Helper()
	for _, key := range []string{"image_available", "file_available"} {
		if _, exists := message[key]; exists {
			t.Fatalf("message still exposes %s: %#v", key, message)
		}
	}
	if _, exists := item["available"]; exists {
		t.Fatalf("media item still exposes available: %#v", item)
	}
}

func TestSendMessageRejectsMissingContextLegacyTextAndUnsupportedMedia(t *testing.T) {
	server, messages, outbound, _ := testServer(t)
	messages.initialized = true
	textItem := []any{map[string]any{"type": textItemType, "text_item": map[string]any{"text": "hello"}}}
	missing := perform(server, http.MethodPost, "/ilink/bot/sendmessage", map[string]any{"msg": map[string]any{"item_list": textItem}}, true)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", missing.Code)
	}
	legacy := perform(server, http.MethodPost, "/ilink/bot/sendmessage", map[string]any{"msg": map[string]any{
		"context_token": server.contextToken("wxid-a"), "text": "hello",
	}}, true)
	if legacy.Code != http.StatusBadRequest {
		t.Fatalf("legacy status=%d", legacy.Code)
	}

	media := perform(server, http.MethodPost, "/ilink/bot/sendmessage", map[string]any{"msg": map[string]any{
		"context_token": server.contextToken("wxid-a"),
		"item_list":     []any{map[string]any{"type": 3, "voice_item": map[string]any{}}},
	}}, true)
	if media.Code != http.StatusNotImplemented || outbound.calls != 0 {
		t.Fatalf("status=%d calls=%d", media.Code, outbound.calls)
	}
}

func TestBusinessRoutesRequireStandardILinkHeaders(t *testing.T) {
	server, _, _, _ := testServer(t)
	response := perform(server, http.MethodPost, "/ilink/bot/msg/notifystart", map[string]any{}, false)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestGetUserInfoReturnsCurrentWeChatAccount(t *testing.T) {
	server, messages, _, _ := testServer(t)
	messages.initialized = true
	messages.userInfo = wechat.UserInfo{AccountID: "wxid-self", WeChatID: "jlyfish", Nickname: "小鱼", AvatarURL: "https://example.test/avatar.png"}
	response := perform(server, http.MethodGet, "/ilink/bot/userinfo", nil, true)
	body := responseJSON(t, response)
	if response.Code != http.StatusOK || body["ret"] != float64(0) || body["account_id"] != "wxid-self" || body["wechat_id"] != "jlyfish" || body["nickname"] != "小鱼" || body["avatar_url"] != "https://example.test/avatar.png" {
		t.Fatalf("status=%d body=%#v", response.Code, body)
	}
}

func TestGetUserInfoRequiresAuthenticationAndReadySession(t *testing.T) {
	server, messages, _, _ := testServer(t)
	unauthorized := perform(server, http.MethodGet, "/ilink/bot/userinfo", nil, false)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}
	unavailable := perform(server, http.MethodGet, "/ilink/bot/userinfo", nil, true)
	body := responseJSON(t, unavailable)
	if unavailable.Code != http.StatusOK || body["ret"] != float64(-14) {
		t.Fatalf("unavailable status=%d body=%#v", unavailable.Code, body)
	}
	messages.initialized = true
	incomplete := perform(server, http.MethodGet, "/ilink/bot/userinfo", nil, true)
	body = responseJSON(t, incomplete)
	if incomplete.Code != http.StatusOK || body["ret"] != float64(-14) {
		t.Fatalf("incomplete status=%d body=%#v", incomplete.Code, body)
	}
}

func TestGetConfigIssuesBoundTypingTicket(t *testing.T) {
	server, _, _, _ := testServer(t)
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

func testServer(t *testing.T) (*Server, *fakeMessages, *fakeSender, *fakeQR) {
	t.Helper()
	messages := &fakeMessages{accountID: "wxid-self"}
	outbound := &fakeSender{}
	qr := &fakeQR{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	media, err := sharedmedia.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := New("api-token", "", media, messages, outbound, qr, logger)
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

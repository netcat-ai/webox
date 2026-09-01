package wechatapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/netcat-ai/webox/internal/sender"
	"github.com/netcat-ai/webox/internal/sharedmedia"
	"github.com/netcat-ai/webox/internal/wechat"
	"github.com/netcat-ai/webox/internal/wechatdb"
	"github.com/netcat-ai/webox/wecom"
)

type roomQuery struct {
	roomID string
	after  int64
	before int64
}

type fakeMessages struct {
	initialized  bool
	userInfo     wechat.UserInfo
	userInfoErr  error
	contacts     []wechatdb.Contact
	contactsErr  error
	remark       string
	sessions     map[string]int64
	sessionAfter int64
	rooms        map[string][]wecom.Message
	queries      []roomQuery
	media        map[string]*wechatdb.MediaFile
	imageRoomID  string
	imageLocalID int64
	imageTime    int64
	imageMD5     string
	files        map[string]*wechatdb.LocalFile
	fileRoomID   string
	fileLocalID  int64
	fileTime     int64
	fileName     string
}

func (source *fakeMessages) IsInitialized() bool { return source.initialized }
func (source *fakeMessages) UserInfo() (wechat.UserInfo, error) {
	return source.userInfo, source.userInfoErr
}
func (source *fakeMessages) ContactsByRemark(remark string) ([]wechatdb.Contact, error) {
	source.remark = remark
	return source.contacts, source.contactsErr
}
func (source *fakeMessages) RoomSessions(after int64) (map[string]int64, error) {
	source.sessionAfter = after
	sessions := make(map[string]int64)
	for roomID, last := range source.sessions {
		if last >= after {
			sessions[roomID] = last
		}
	}
	return sessions, nil
}
func (source *fakeMessages) RoomMessages(roomID string, after int64, limit int) ([]wecom.Message, error) {
	return nil, nil
}
func (source *fakeMessages) RoomMessagesBySort(roomID string, after, before int64) ([]wecom.Message, error) {
	source.queries = append(source.queries, roomQuery{roomID: roomID, after: after, before: before})
	var messages []wecom.Message
	for _, message := range source.rooms[roomID] {
		if message.SortSequence > after && message.SortSequence < before {
			messages = append(messages, message)
		}
	}
	return messages, nil
}
func (source *fakeMessages) ReadImage(roomID string, localID, createTime int64, md5 string) (*wechatdb.MediaFile, error) {
	source.imageRoomID = roomID
	source.imageLocalID = localID
	source.imageTime = createTime
	source.imageMD5 = md5
	return source.media[roomID+"/"+md5], nil
}

func (source *fakeMessages) ReadFile(roomID string, localID, createTime int64, filename string) (*wechatdb.LocalFile, error) {
	source.fileRoomID = roomID
	source.fileLocalID = localID
	source.fileTime = createTime
	source.fileName = filename
	return source.files[roomID+"/"+filename], nil
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

func TestLegacyWeComRouteIsAbsent(t *testing.T) {
	server, _, _ := testServer(t)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/wecom", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestHealthOnlyExposesReadiness(t *testing.T) {
	server, _, _ := testServer(t)
	response := perform(server, http.MethodGet, "/healthz", nil, false)
	body := responseJSON(t, response)
	if response.Code != http.StatusOK || body["ok"] != true || body["ready"] != false || len(body) != 2 {
		t.Fatalf("status=%d body=%#v", response.Code, body)
	}
}

func TestGetContactsReturnsExactRemarkMatches(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	messages.contacts = []wechatdb.Contact{{
		RoomID: "50261801724@chatroom", Remark: "webox.test", Nickname: "测试群",
	}}

	response := perform(server, http.MethodGet, "/ilink/bot/contacts?remark=webox.test", nil, true)
	body := responseJSON(t, response)
	contacts := body["contacts"].([]any)
	contact := contacts[0].(map[string]any)
	if response.Code != http.StatusOK || body["ret"] != float64(0) || messages.remark != "webox.test" ||
		len(contacts) != 1 || contact["roomid"] != "50261801724@chatroom" ||
		contact["remark"] != "webox.test" || contact["nickname"] != "测试群" {
		t.Fatalf("status=%d body=%#v remark=%q", response.Code, body, messages.remark)
	}
}

func TestGetContactsRequiresAuthenticatedNonEmptyRemark(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	if response := perform(server, http.MethodGet, "/ilink/bot/contacts?remark=webox.test", nil, false); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", response.Code)
	}
	if response := perform(server, http.MethodGet, "/ilink/bot/contacts", nil, true); response.Code != http.StatusBadRequest {
		t.Fatalf("missing remark status=%d", response.Code)
	}
}

func TestCompatibilityRoutesAreRemoved(t *testing.T) {
	server, _, _ := testServer(t)
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/ilink/bot/get_bot_qrcode"},
		{http.MethodPost, "/ilink/bot/get_bot_qrcode"},
		{http.MethodGet, "/ilink/bot/get_qrcode_status"},
		{http.MethodPost, "/ilink/bot/getconfig"},
		{http.MethodPost, "/ilink/bot/sendtyping"},
		{http.MethodPost, "/ilink/bot/msg/notifystart"},
		{http.MethodPost, "/ilink/bot/msg/notifystop"},
	}
	for _, test := range tests {
		response := perform(server, test.method, test.path, nil, false)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s %s: status=%d", test.method, test.path, response.Code)
		}
	}
}

func TestGetUpdatesUsesGlobalSortCursorAndOneHourWindow(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	server.now = func() time.Time { return time.Unix(20_000, 0) }
	messages.sessions = map[string]int64{"room-a": 10_000, "room-b": 10_100}
	messages.rooms = map[string][]wecom.Message{
		"room-a": {{MsgID: "a", RoomID: "room-a", MsgType: "text", SortSequence: 10_001_000, Sequence: 2, Text: &wecom.Text{Content: "a"}}},
		"room-b": {{MsgID: "b", RoomID: "room-b", MsgType: "text", SortSequence: 10_000_500, Sequence: 1, Text: &wecom.Text{Content: "b"}}},
	}
	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"cursor": "9999999"}, true)
	body := responseJSON(t, response)
	items := body["messages"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["msgid"] != "b" || items[1].(map[string]any)["msgid"] != "a" {
		t.Fatalf("body=%#v", body)
	}
	if body["cursor"] != "10100999" {
		t.Fatalf("cursor=%v", body["cursor"])
	}
	if messages.sessionAfter != 9_999 {
		t.Fatalf("session after=%d", messages.sessionAfter)
	}
}

func TestEmptyCursorStartsFiveDaysAgo(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cursor, err := parseCursor("", now)
	if err != nil || cursor != now.Add(-5*24*time.Hour).Unix()*1000-1 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
}

func TestGetUpdatesRejectsInvalidCursor(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"cursor": "bad"}, true)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSendMessageUsesRoomRoutingAndReturnsMessageID(t *testing.T) {
	server, messages, outbound := testServer(t)
	messages.initialized = true
	body := map[string]any{"msgs": []any{map[string]any{
		"msgid": "request-1", "action": "send", "from": "wxid-self", "tolist": []string{},
		"roomid": "group@chatroom", "msgtime": 1781703356000, "msgtype": "text",
		"text": map[string]any{"content": "reply"},
	}}}

	first := perform(server, http.MethodPost, "/ilink/bot/sendmessage", body, true)
	if result := responseJSON(t, first); first.Code != http.StatusOK || result["client_msg_id"] != "request-1" {
		t.Fatalf("status=%d result=%#v", first.Code, result)
	}
	body["msgs"].([]any)[0].(map[string]any)["text"] = map[string]any{"content": "changed"}
	second := perform(server, http.MethodPost, "/ilink/bot/sendmessage", body, true)
	if result := responseJSON(t, second); second.Code != http.StatusOK || result["client_msg_id"] != "request-1" || outbound.calls != 2 || outbound.target != "group@chatroom" || outbound.text != "changed" {
		t.Fatalf("status=%d result=%#v sender=%#v", second.Code, result, outbound)
	}
}

func TestSendMessageBatchesTextImageAndFile(t *testing.T) {
	server, messages, outbound := testServer(t)
	messages.initialized = true
	body := map[string]any{"msgs": []any{
		map[string]any{"msgid": "1", "roomid": "group@chatroom", "msgtype": "file", "file": map[string]any{"sdkfileid": "outbox/report.txt"}},
		map[string]any{"msgid": "2", "roomid": "group@chatroom", "msgtype": "text", "text": map[string]any{"content": "文件已整理完成"}},
		map[string]any{"msgid": "3", "roomid": "group@chatroom", "msgtype": "image", "image": map[string]any{"sdkfileid": "outbox/chart.png"}},
		map[string]any{"msgid": "4", "roomid": "group@chatroom", "msgtype": "text", "text": map[string]any{"content": "请查收"}},
	}}

	response := perform(server, http.MethodPost, "/ilink/bot/sendmessage", body, true)
	if response.Code != http.StatusOK || outbound.calls != 1 || len(outbound.items) != 4 {
		t.Fatalf("status=%d sender=%#v", response.Code, outbound)
	}
	if outbound.items[0].Kind != "file" || outbound.items[0].SharedPath != "outbox/report.txt" || outbound.items[1].Kind != "text" || outbound.items[1].Text != "文件已整理完成" || outbound.items[2].Kind != "image" || outbound.items[2].SharedPath != "outbox/chart.png" || outbound.items[3].Kind != "text" || outbound.items[3].Text != "请查收" {
		t.Fatalf("items=%#v", outbound.items)
	}
}

func TestSharedMediaIsSentForEveryRequest(t *testing.T) {
	server, messages, outbound := testServer(t)
	messages.initialized = true
	image := map[string]any{"msgs": []any{map[string]any{
		"msgid": "image-request-1", "roomid": "group@chatroom", "msgtype": "image",
		"image": map[string]any{"sdkfileid": "outbox/reply.png"},
	}}}
	file := map[string]any{"msgs": []any{map[string]any{
		"msgid": "file-request-1", "roomid": "group@chatroom", "msgtype": "file",
		"file": map[string]any{"sdkfileid": "outbox/report.pdf"},
	}}}
	for range 2 {
		if response := perform(server, http.MethodPost, "/ilink/bot/sendmessage", image, true); response.Code != http.StatusOK {
			t.Fatalf("image status=%d", response.Code)
		}
		if response := perform(server, http.MethodPost, "/ilink/bot/sendmessage", file, true); response.Code != http.StatusOK {
			t.Fatalf("file status=%d", response.Code)
		}
	}
	if outbound.imageCalls != 2 || outbound.fileCalls != 2 || outbound.target != "group@chatroom" || outbound.imagePath != "outbox/reply.png" || outbound.filePath != "outbox/report.pdf" {
		t.Fatalf("sender=%#v", outbound)
	}
}

func TestSendMessageRejectsMissingTargetLegacyEnvelopeAndUnsupportedMedia(t *testing.T) {
	server, messages, outbound := testServer(t)
	messages.initialized = true
	missing := perform(server, http.MethodPost, "/ilink/bot/sendmessage", map[string]any{"msgs": []any{map[string]any{
		"msgtype": "text", "text": map[string]any{"content": "hello"},
	}}}, true)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", missing.Code)
	}
	nonEmptyToList := perform(server, http.MethodPost, "/ilink/bot/sendmessage", map[string]any{"msgs": []any{map[string]any{
		"roomid": "wxid-a", "tolist": []string{"wxid-b"}, "msgtype": "text", "text": map[string]any{"content": "hello"},
	}}}, true)
	if nonEmptyToList.Code != http.StatusBadRequest {
		t.Fatalf("non-empty tolist status=%d", nonEmptyToList.Code)
	}
	legacy := perform(server, http.MethodPost, "/ilink/bot/sendmessage", map[string]any{"msg": map[string]any{"msgtype": "text"}}, true)
	if legacy.Code != http.StatusBadRequest {
		t.Fatalf("legacy status=%d", legacy.Code)
	}
	media := perform(server, http.MethodPost, "/ilink/bot/sendmessage", map[string]any{"msgs": []any{map[string]any{
		"roomid": "wxid-a", "tolist": []string{}, "msgtype": "voice", "voice": map[string]any{},
	}}}, true)
	if media.Code != http.StatusNotImplemented || outbound.calls != 0 {
		t.Fatalf("status=%d calls=%d", media.Code, outbound.calls)
	}
}

func TestBusinessRoutesRequireStandardILinkHeaders(t *testing.T) {
	server, _, _ := testServer(t)
	if response := perform(server, http.MethodGet, "/ilink/bot/userinfo", nil, false); response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestGetUserInfoReturnsCurrentWeChatAccount(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	messages.userInfo = wechat.UserInfo{AccountID: "wxid-self", WeChatID: "jlyfish", Nickname: "小鱼", AvatarURL: "https://example.test/avatar.png"}
	response := perform(server, http.MethodGet, "/ilink/bot/userinfo", nil, true)
	body := responseJSON(t, response)
	if response.Code != http.StatusOK || body["ret"] != float64(0) || body["account_id"] != "wxid-self" || body["wechat_id"] != "jlyfish" || body["nickname"] != "小鱼" || body["avatar_url"] != "https://example.test/avatar.png" {
		t.Fatalf("status=%d body=%#v", response.Code, body)
	}
}

func TestGetUserInfoRequiresAuthenticationAndReadySession(t *testing.T) {
	server, messages, _ := testServer(t)
	if unauthorized := perform(server, http.MethodGet, "/ilink/bot/userinfo", nil, false); unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}
	unavailable := perform(server, http.MethodGet, "/ilink/bot/userinfo", nil, true)
	if body := responseJSON(t, unavailable); unavailable.Code != http.StatusOK || body["ret"] != float64(-14) {
		t.Fatalf("unavailable status=%d body=%#v", unavailable.Code, body)
	}
	messages.initialized = true
	incomplete := perform(server, http.MethodGet, "/ilink/bot/userinfo", nil, true)
	if body := responseJSON(t, incomplete); incomplete.Code != http.StatusOK || body["ret"] != float64(-14) {
		t.Fatalf("incomplete status=%d body=%#v", incomplete.Code, body)
	}
}

func testServer(t *testing.T) (*Server, *fakeMessages, *fakeSender) {
	t.Helper()
	messages := &fakeMessages{}
	outbound := &fakeSender{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	media, err := sharedmedia.New(filepath.Join(t.TempDir(), "shared"))
	if err != nil {
		t.Fatal(err)
	}
	return New("api-token", media, messages, outbound, logger), messages, outbound
}

func messagesForTest(t *testing.T, room any) []map[string]any {
	t.Helper()
	raw := room.(map[string]any)["messages"].([]any)
	messages := make([]map[string]any, len(raw))
	for index := range raw {
		messages[index] = raw[index].(map[string]any)
	}
	return messages
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

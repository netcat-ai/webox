package ilink

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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
	limit  int
}

type fakeMessages struct {
	initialized  bool
	userInfo     wechat.UserInfo
	userInfoErr  error
	contacts     []wechatdb.Contact
	contactsErr  error
	remark       string
	sessions     map[string]int64
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
func (source *fakeMessages) RoomSessions() (map[string]int64, error) {
	return source.sessions, nil
}
func (source *fakeMessages) RoomMessages(roomID string, after int64, limit int) ([]wecom.Message, error) {
	source.queries = append(source.queries, roomQuery{roomID: roomID, after: after, limit: limit})
	var messages []wecom.Message
	for _, message := range source.rooms[roomID] {
		if message.Sequence > after {
			messages = append(messages, message)
		}
		if len(messages) == limit {
			break
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

func TestWeComRouteIsRemoved(t *testing.T) {
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

func TestGetUpdatesUsesPerRoomSequencesAndDiscoversNewRooms(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	messages.sessions = map[string]int64{
		"room-1": 9,
		"room-2": 10,
		"room-3": 150,
	}
	messages.rooms = map[string][]wecom.Message{
		"room-1": {{MsgID: "m8", RoomID: "room-1", MsgType: wecom.MessageTypeText, Text: &wecom.Text{Content: "one"}, Sequence: 8}},
		"room-3": {{MsgID: "m51", RoomID: "room-3", MsgType: wecom.MessageTypeText, Text: &wecom.Text{Content: "new"}, Sequence: 51}},
	}

	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{
		"rooms": map[string]any{
			"room-1": map[string]any{"seq": 7},
			"room-2": map[string]any{"seq": 10},
		},
	}, true)
	body := responseJSON(t, response)
	rooms := body["rooms"].(map[string]any)
	if response.Code != http.StatusOK || body["ret"] != float64(0) || len(rooms) != 2 {
		t.Fatalf("status=%d body=%#v", response.Code, body)
	}
	if _, exists := rooms["room-2"]; exists {
		t.Fatalf("unchanged room was returned: %#v", rooms)
	}
	if messagesForTest(t, rooms["room-1"])[0]["seq"] != float64(8) || messagesForTest(t, rooms["room-3"])[0]["seq"] != float64(51) {
		t.Fatalf("rooms=%#v", rooms)
	}
	if len(messages.queries) != 2 || messages.queries[0] != (roomQuery{roomID: "room-1", after: 7, limit: 100}) || messages.queries[1] != (roomQuery{roomID: "room-3", after: 50, limit: 100}) {
		t.Fatalf("queries=%+v", messages.queries)
	}
}

func TestGetUpdatesReturnsReadyPrefixesWithoutBlockingOtherRooms(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	now := time.Now().UnixMilli()
	messages.sessions = map[string]int64{"room-a": 3, "room-b": 1}
	messages.rooms = map[string][]wecom.Message{
		"room-a": {
			{MsgID: "a1", RoomID: "room-a", MsgType: wecom.MessageTypeText, Text: &wecom.Text{Content: "ready"}, MsgTime: now, Sequence: 1},
			{MsgID: "a2", RoomID: "room-a", MsgType: wecom.MessageTypeImage, Image: &wecom.Image{}, MsgTime: now, Sequence: 2},
			{MsgID: "a3", RoomID: "room-a", MsgType: wecom.MessageTypeText, Text: &wecom.Text{Content: "blocked"}, MsgTime: now, Sequence: 3},
		},
		"room-b": {{MsgID: "b1", RoomID: "room-b", MsgType: wecom.MessageTypeText, Text: &wecom.Text{Content: "independent"}, MsgTime: now, Sequence: 1}},
	}

	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{
		"rooms": map[string]any{
			"room-a": map[string]any{"seq": 0},
			"room-b": map[string]any{"seq": 0},
		},
	}, true)
	rooms := responseJSON(t, response)["rooms"].(map[string]any)
	roomA := messagesForTest(t, rooms["room-a"])
	roomB := messagesForTest(t, rooms["room-b"])
	if len(roomA) != 1 || roomA[0]["msgid"] != "a1" || len(roomB) != 1 || roomB[0]["msgid"] != "b1" {
		t.Fatalf("rooms=%#v", rooms)
	}
}

func TestGetUpdatesLocatesFileFromConvertedMessage(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	createdAt := time.Now().UnixMilli()
	path := filepath.Join(t.TempDir(), "stored-report.pdf")
	if err := os.WriteFile(path, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	messages.sessions = map[string]int64{"room-a": 7}
	messages.rooms = map[string][]wecom.Message{"room-a": {{
		MsgID: "file-1", RoomID: "room-a", MsgType: wecom.MessageTypeFile,
		MsgTime: createdAt, Sequence: 7, File: &wecom.File{FileName: "report.pdf"},
	}}}
	messages.files = map[string]*wechatdb.LocalFile{
		"room-a/report.pdf": {Path: path, Filename: "report.pdf"},
	}

	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{
		"rooms": map[string]any{"room-a": map[string]any{"seq": 6}},
	}, true)
	room := responseJSON(t, response)["rooms"].(map[string]any)["room-a"]
	file := messagesForTest(t, room)[0]["file"].(map[string]any)
	if response.Code != http.StatusOK || file["filename"] != "report.pdf" || file["sdkfileid"] == "" {
		t.Fatalf("status=%d file=%#v", response.Code, file)
	}
	if messages.fileRoomID != "room-a" || messages.fileLocalID != 7 || messages.fileTime != time.UnixMilli(createdAt).Unix() || messages.fileName != "report.pdf" {
		t.Fatalf("file query room=%q localID=%d time=%d name=%q", messages.fileRoomID, messages.fileLocalID, messages.fileTime, messages.fileName)
	}
}

func TestGetUpdatesLocatesImageFromConvertedMessage(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	createdAt := time.Now().UnixMilli()
	md5sum := "0123456789abcdef0123456789abcdef"
	messages.sessions = map[string]int64{"room-a": 8}
	messages.rooms = map[string][]wecom.Message{"room-a": {{
		MsgID: "image-1", RoomID: "room-a", MsgType: wecom.MessageTypeImage,
		MsgTime: createdAt, Sequence: 8, Image: &wecom.Image{MD5Sum: md5sum},
	}}}
	messages.media = map[string]*wechatdb.MediaFile{
		"room-a/" + md5sum: {Data: []byte("image"), ContentType: "image/png"},
	}

	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{
		"rooms": map[string]any{"room-a": map[string]any{"seq": 7}},
	}, true)
	room := responseJSON(t, response)["rooms"].(map[string]any)["room-a"]
	image := messagesForTest(t, room)[0]["image"].(map[string]any)
	if response.Code != http.StatusOK || image["sdkfileid"] == "" {
		t.Fatalf("status=%d image=%#v", response.Code, image)
	}
	if messages.imageRoomID != "room-a" || messages.imageLocalID != 8 || messages.imageTime != time.UnixMilli(createdAt).Unix() || messages.imageMD5 != md5sum {
		t.Fatalf("image query room=%q localID=%d time=%d md5=%q", messages.imageRoomID, messages.imageLocalID, messages.imageTime, messages.imageMD5)
	}
}

func TestGetUpdatesRejectsNegativeSequence(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{
		"rooms": map[string]any{"room-a": map[string]any{"seq": -1}},
	}, true)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSendMessageUsesWeComRoutingAndReturnsMessageID(t *testing.T) {
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

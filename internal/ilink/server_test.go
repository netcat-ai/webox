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
	"strings"
	"testing"
	"time"

	"github.com/netcat-ai/webox/internal/sender"
	"github.com/netcat-ai/webox/internal/sharedmedia"
	"github.com/netcat-ai/webox/internal/wechat"
	"github.com/netcat-ai/webox/internal/wechatdb"
	"github.com/netcat-ai/webox/wecom"
)

type fakeMessages struct {
	initialized bool
	accountID   string
	userInfo    wechat.UserInfo
	userInfoErr error
	validateErr error
	pollResult  wechat.PollResult
	pollErr     error
	media       *wechatdb.MediaFile
	mediaErr    error
	file        *wechatdb.LocalFile
	fileErr     error
}

func inboundMessage(id, room, senderID string, createdAt int64, body wecom.Message) wecom.Message {
	body.MsgID = id
	body.Action = wecom.ActionSend
	body.From = senderID
	body.ToList = []string{}
	body.RoomID = room
	body.MsgTime = createdAt
	body.Sequence = 7
	return body
}

func textItem(text string) wecom.Message {
	return wecom.Message{MsgType: wecom.MessageTypeText, Text: &wecom.Text{Content: text}}
}

func imageItem() wecom.Message {
	return wecom.Message{MsgType: wecom.MessageTypeImage, Image: &wecom.Image{}}
}

func fileItem(filename string) wecom.Message {
	return wecom.Message{MsgType: wecom.MessageTypeFile, File: &wecom.File{FileName: filename}}
}

func (source *fakeMessages) IsInitialized() bool { return source.initialized }
func (source *fakeMessages) UserInfo() (wechat.UserInfo, error) {
	return source.userInfo, source.userInfoErr
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

func TestGetUpdatesUsesUnifiedRoomTextMessageFormat(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	messages.accountID = "wxid-stale"
	messages.pollResult = wechat.PollResult{AccountID: "wxid-self", Cursor: "next-cursor", Messages: []wecom.Message{
		inboundMessage("message-1", "wxid-alice", "wxid-alice", 1781703356000, textItem("hello")),
	}}

	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": "", "base_info": map[string]any{}}, true)
	body := responseJSON(t, response)
	if response.Code != http.StatusOK || body["ret"] != float64(0) || body["get_updates_buf"] != "next-cursor" {
		t.Fatalf("status=%d body=%#v", response.Code, body)
	}
	messagesView := body["msgs"].([]any)
	message := messagesView[0].(map[string]any)
	text := message["text"].(map[string]any)["content"]
	toList := message["tolist"].([]any)
	if message["msgid"] != "message-1" || message["action"] != "send" || message["from"] != "wxid-alice" ||
		len(toList) != 0 || message["roomid"] != "wxid-alice" ||
		message["msgtype"] != "text" || text != "hello" {
		t.Fatalf("message=%#v", message)
	}
	for _, key := range []string{"item_list", "context_token", "mentioned_me", "is_completed", "shared_path"} {
		if _, exists := message[key]; exists {
			t.Fatalf("non-WeCom field %q in message=%#v", key, message)
		}
	}
	if len(message) != 8 {
		t.Fatalf("message has unexpected fields: %#v", message)
	}
}

func TestGetUpdatesUsesRoomIDForOutgoingPrivateMessage(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	message := inboundMessage("outgoing-1", "wxid-alice", "wxid-alice", 1781703356000, textItem("sent"))
	message.Outgoing = true
	messages.pollResult = wechat.PollResult{AccountID: "wxid-self", Cursor: "next", Messages: []wecom.Message{message}}

	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": ""}, true)
	body := responseJSON(t, response)
	view := body["msgs"].([]any)[0].(map[string]any)
	toList := view["tolist"].([]any)
	if response.Code != http.StatusOK || view["from"] != "wxid-self" || len(toList) != 0 || view["roomid"] != "wxid-alice" {
		t.Fatalf("message=%#v", view)
	}
}

func TestGetUpdatesIncludesOrdinaryLinkTitleDescriptionAndURL(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	messages.pollResult = wechat.PollResult{AccountID: "wxid-self", Cursor: "next-cursor", Messages: []wecom.Message{
		inboundMessage("101", "wxid-alice", "wxid-alice", 1781703356000, wecom.Message{
			MsgType: wecom.MessageTypeLink,
			Link: &wecom.Link{
				Title: "文章标题", Description: "文章摘要",
				LinkURL: "https://example.com/article?id=1&from=wechat",
			},
		}),
	}}

	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": ""}, true)
	body := responseJSON(t, response)
	message := body["msgs"].([]any)[0].(map[string]any)
	link := message["link"].(map[string]any)
	if response.Code != http.StatusOK || message["msgtype"] != "link" ||
		link["title"] != "文章标题" || link["description"] != "文章摘要" ||
		link["link_url"] != "https://example.com/article?id=1&from=wechat" {
		t.Fatalf("status=%d message=%#v want typed link", response.Code, message)
	}
	if _, exists := message["text"]; exists {
		t.Fatalf("link was flattened into text: %#v", message)
	}
}

func TestGetUpdatesPreservesSphFeedMessage(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	messages.accountID = "wxid-self"
	messages.pollResult = wechat.PollResult{AccountID: "wxid-self", Cursor: "next-cursor", Messages: []wecom.Message{{
		MsgID: "8419589511486552249", Action: "send", From: "webox.xiaxia", ToList: []string{},
		RoomID: "webox.xiaxia", MsgTime: 1781703356000, MsgType: wecom.MessageTypeSphFeed,
		SphFeed: &wecom.SphFeed{FeedType: 4, SphName: "黄同学的移动小屋", FeedDesc: "自驾游装备收纳清单"},
	}}}

	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": ""}, true)
	body := responseJSON(t, response)
	message := body["msgs"].([]any)[0].(map[string]any)
	feed := message["sphfeed"].(map[string]any)
	if response.Code != http.StatusOK || message["msgtype"] != "sphfeed" || feed["feed_type"] != float64(4) ||
		feed["sph_name"] != "黄同学的移动小屋" || feed["feed_desc"] != "自驾游装备收纳清单" {
		t.Fatalf("status=%d message=%#v", response.Code, message)
	}
	if _, exists := message["text"]; exists {
		t.Fatalf("sphfeed was flattened into text: %#v", message)
	}
	for _, key := range []string{"object_id", "media_list"} {
		if _, exists := feed[key]; exists {
			t.Fatalf("non-WeCom sphfeed field %q: %#v", key, feed)
		}
	}
}

func TestGetUpdatesUsesWeComMixedForQuotedImage(t *testing.T) {
	server, messages, _ := testServer(t)
	mediaRoot := t.TempDir()
	mediaStore, err := sharedmedia.New(mediaRoot)
	if err != nil {
		t.Fatal(err)
	}
	server.media = mediaStore
	messages.initialized = true
	imageBytes := append([]byte{0xff, 0xd8, 0xff, 0xe0}, []byte("quoted-image")...)
	messages.media = &wechatdb.MediaFile{Data: imageBytes, ContentType: "image/jpeg", Filename: "quoted.jpg"}
	itemValue := wecom.Message{MsgType: wecom.MessageTypeMixed, Mixed: &wecom.Mixed{Items: []wecom.MixedItem{
		{Type: wecom.MessageTypeText, Content: "虾虾 看看这个"},
		{Type: wecom.MessageTypeImage, MessageID: "3143822696652695030"},
	}}}
	messages.pollResult = wechat.PollResult{AccountID: "wxid-self", Cursor: "next-cursor", Messages: []wecom.Message{
		inboundMessage("quote-1", "wxid-alice", "wxid-alice", time.Now().UnixMilli(), itemValue),
	}}

	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": ""}, true)
	body := responseJSON(t, response)
	message := body["msgs"].([]any)[0].(map[string]any)
	items := message["mixed"].(map[string]any)["item"].([]any)
	text := items[0].(map[string]any)
	imageItem := items[1].(map[string]any)
	sharedPath := imageItem["sdkfileid"].(string)
	if response.Code != http.StatusOK || message["msgtype"] != "mixed" || text["content"] != "虾虾 看看这个" {
		t.Fatalf("status=%d message=%#v", response.Code, message)
	}
	written, err := os.ReadFile(filepath.Join(mediaRoot, filepath.FromSlash(sharedPath)))
	if err != nil || !bytes.Equal(written, imageBytes) {
		t.Fatalf("path=%q data=%x err=%v", sharedPath, written, err)
	}
}

func TestGetUpdatesDoesNotAddMentionExtension(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	firstMessage := inboundMessage("message-1", "group@chatroom", "wxid-alice", 1781703356000, textItem("@Self hello"))
	secondMessage := inboundMessage("message-2", "group@chatroom", "wxid-alice", 1781703357000, textItem("@all hello"))
	messages.pollResult = wechat.PollResult{AccountID: "wxid-self", Cursor: "next-cursor", Messages: []wecom.Message{firstMessage, secondMessage}}

	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": ""}, true)
	body := responseJSON(t, response)
	views := body["msgs"].([]any)
	first := views[0].(map[string]any)
	second := views[1].(map[string]any)
	_, firstMentioned := first["mentioned_me"]
	_, secondMentioned := second["mentioned_me"]
	if response.Code != http.StatusOK || firstMentioned || secondMentioned {
		t.Fatalf("status=%d messages=%#v", response.Code, views)
	}
}

func TestGetUpdatesLogsMessagesSkippedForUnresolvedSenders(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	messages.pollResult = wechat.PollResult{
		AccountID: "wxid-self",
		Cursor:    "next-cursor",
		Messages: []wecom.Message{
			inboundMessage("message-1", "wxid-alice", "wxid-alice", 1781703356000, textItem("hello")),
		},
		Skipped: []wechatdb.SkippedMessage{{
			MessageID: "message-skipped", Shard: "message/message_0.db", RealSenderID: 42,
		}},
	}
	var logs bytes.Buffer
	server.logger = slog.New(slog.NewTextHandler(&logs, nil))

	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{
		"get_updates_buf": "cursor",
	}, true)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	output := logs.String()
	for _, fragment := range []string{
		"skipping WeChat message with unresolved sender",
		"msgid=message-skipped",
		"message_shard=message/message_0.db",
		"real_sender_id=42",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("log %q does not contain %q", output, fragment)
		}
	}
}

func TestGetUpdatesRejectsInvalidCursorAndReportsExpiredSession(t *testing.T) {
	server, messages, _ := testServer(t)
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

func TestSharedImageIsSentForEveryRequest(t *testing.T) {
	server, messages, outbound := testServer(t)
	messages.initialized = true
	body := map[string]any{"msgs": []any{map[string]any{
		"msgid": "image-request-1", "roomid": "group@chatroom", "msgtype": "image",
		"image": map[string]any{"sdkfileid": "outbox/reply.png"},
	}}}
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
	server, messages, _ := testServer(t)
	mediaRoot := t.TempDir()
	mediaStore, err := sharedmedia.New(mediaRoot)
	if err != nil {
		t.Fatal(err)
	}
	server.media = mediaStore
	messages.initialized = true
	imageBytes := append([]byte{0xff, 0xd8, 0xff, 0xe0}, []byte("image-bytes")...)
	messages.media = &wechatdb.MediaFile{Data: imageBytes, ContentType: "image/jpeg", Filename: "message.jpg"}
	messages.pollResult = wechat.PollResult{Cursor: "next", Messages: []wecom.Message{
		inboundMessage("123", "wxid-a", "wxid-a", 1781703356000, imageItem()),
	}}
	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": ""}, true)
	body := responseJSON(t, response)
	message := body["msgs"].([]any)[0].(map[string]any)
	imageItem := message["image"].(map[string]any)
	sharedPath := imageItem["sdkfileid"].(string)
	if _, topLevel := message["shared_path"]; topLevel || imageItem["sdkfileid"] != sharedPath {
		t.Fatalf("message=%#v", message)
	}
	assertNoMediaAvailabilityFields(t, message, imageItem)
	written, err := os.ReadFile(filepath.Join(mediaRoot, filepath.FromSlash(sharedPath)))
	if err != nil || !bytes.Equal(written, imageBytes) {
		t.Fatalf("path=%q data=%x err=%v", sharedPath, written, err)
	}
}

func TestIncomingMediaWaitsWithoutAdvancingCursor(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	now := time.Now().UnixMilli()
	messages.pollResult = wechat.PollResult{Cursor: "next", Messages: []wecom.Message{
		inboundMessage("image-pending", "wxid-a", "wxid-a", now, imageItem()),
		inboundMessage("file-pending", "wxid-a", "wxid-a", now, fileItem("pending.zip")),
	}}
	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": "cursor-original"}, true)
	body := responseJSON(t, response)
	if response.Code != http.StatusOK || body["get_updates_buf"] != "cursor-original" || len(body["msgs"].([]any)) != 0 {
		t.Fatalf("pending status=%d body=%#v", response.Code, body)
	}
}

func TestSharedFileIsSentForEveryRequest(t *testing.T) {
	server, messages, outbound := testServer(t)
	messages.initialized = true
	body := map[string]any{"msgs": []any{map[string]any{
		"msgid": "file-request-1", "roomid": "group@chatroom", "msgtype": "file",
		"file": map[string]any{"sdkfileid": "outbox/report.pdf"},
	}}}
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
	server, messages, _ := testServer(t)
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
	messages.pollResult = wechat.PollResult{Cursor: "next", Messages: []wecom.Message{
		inboundMessage("456", "wxid-a", "wxid-a", 1781703356000, fileItem("report.pdf")),
	}}
	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": ""}, true)
	body := responseJSON(t, response)
	message := body["msgs"].([]any)[0].(map[string]any)
	fileItem := message["file"].(map[string]any)
	sharedPath := fileItem["sdkfileid"].(string)
	if _, topLevel := message["shared_path"]; topLevel || fileItem["filename"] != "report.pdf" || fileItem["sdkfileid"] != sharedPath {
		t.Fatalf("message=%#v", message)
	}
	assertNoMediaAvailabilityFields(t, message, fileItem)
	written, err := os.ReadFile(filepath.Join(mediaRoot, filepath.FromSlash(sharedPath)))
	if err != nil || !bytes.Equal(written, fileBytes) {
		t.Fatalf("path=%q data=%q err=%v", sharedPath, written, err)
	}
}

func TestIncomingTimedOutMediaIsDeliveredWithoutSharedPathAndAdvancesCursor(t *testing.T) {
	server, messages, _ := testServer(t)
	messages.initialized = true
	timedOut := time.Now().Add(-inboundMediaWait).UnixMilli()
	messages.pollResult = wechat.PollResult{Cursor: "next", Messages: []wecom.Message{
		inboundMessage("image-timeout", "wxid-a", "wxid-a", timedOut, imageItem()),
		inboundMessage("file-timeout", "wxid-a", "wxid-a", timedOut, fileItem("pending.zip")),
	}}
	response := perform(server, http.MethodPost, "/ilink/bot/getupdates", map[string]any{"get_updates_buf": ""}, true)
	body := responseJSON(t, response)
	views := body["msgs"].([]any)
	if response.Code != http.StatusOK || body["get_updates_buf"] != "next" || len(views) != 2 {
		t.Fatalf("status=%d body=%#v", response.Code, body)
	}
	image := views[0].(map[string]any)
	imageItem := image["image"].(map[string]any)
	if _, topLevel := image["shared_path"]; topLevel || imageItem["sdkfileid"] != "" {
		t.Fatalf("image=%#v", image)
	}
	assertNoMediaAvailabilityFields(t, image, imageItem)
	file := views[1].(map[string]any)
	fileItem := file["file"].(map[string]any)
	if fileItem["filename"] != "pending.zip" || fileItem["sdkfileid"] != "" {
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
	response := perform(server, http.MethodGet, "/ilink/bot/userinfo", nil, false)
	if response.Code != http.StatusUnauthorized {
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

func testServer(t *testing.T) (*Server, *fakeMessages, *fakeSender) {
	t.Helper()
	messages := &fakeMessages{accountID: "wxid-self"}
	outbound := &fakeSender{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	media, err := sharedmedia.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := New("api-token", media, messages, outbound, logger)
	server.pollTimeout = time.Millisecond
	server.pollInterval = time.Millisecond
	return server, messages, outbound
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

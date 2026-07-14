package wecom

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/netcat-ai/webox/internal/sender"
	"github.com/netcat-ai/webox/internal/wechat"
)

type scriptedMessages struct {
	mu      sync.Mutex
	batches []wechat.PollResult
}

func (source *scriptedMessages) IsInitialized() bool { return true }

func (source *scriptedMessages) PollMessages(_ string, _ int) (wechat.PollResult, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	if len(source.batches) == 0 {
		return wechat.PollResult{Cursor: "done", Messages: []map[string]any{}}, nil
	}
	result := source.batches[0]
	source.batches = source.batches[1:]
	return result, nil
}

func TestWebsocketAuthenticatesAndAcknowledgesPing(t *testing.T) {
	queue := make(chan sender.Job, 1)
	server := New("webox", "test-secret", wechat.New(t.TempDir(), "test-token"), queue, slog.Default())
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	url := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	connection, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	writeFrame(t, connection, map[string]any{
		"cmd": subscribe, "headers": map[string]any{"req_id": "auth-1"},
		"body": map[string]any{"bot_id": "webox", "secret": "test-secret"},
	})
	if ack := readFrame(t, connection); integerValue(ack["errcode"]) != 0 || ack["headers"].(map[string]any)["req_id"] != "auth-1" {
		t.Fatalf("unexpected auth ACK: %#v", ack)
	}
	writeFrame(t, connection, map[string]any{"cmd": pingCommand, "headers": map[string]any{"req_id": "ping-1"}})
	if ack := readFrame(t, connection); integerValue(ack["errcode"]) != 0 || ack["headers"].(map[string]any)["req_id"] != "ping-1" {
		t.Fatalf("unexpected ping ACK: %#v", ack)
	}
}

func TestCallbackMapsGroupAndSingleMessages(t *testing.T) {
	group, target, ok := callbackBody("webox", map[string]any{
		"msgid": "42", "from": "wxid_member", "roomid": "123@chatroom",
		"msgtime": int64(1_700_000_000_000), "msgtype": "text", "text": map[string]any{"content": "hello"},
	})
	if !ok || target != "123@chatroom" || group["chattype"] != "group" || group["chatid"] != target {
		t.Fatalf("unexpected group callback: %#v", group)
	}
	single, _, ok := callbackBody("webox", map[string]any{
		"msgid": "43", "from": "wxid_friend", "roomid": "wxid_friend",
		"msgtime": int64(1_700_000_000_000), "msgtype": "text", "text": map[string]any{"content": "hello"},
	})
	if !ok || single["chattype"] != "single" || single["chatid"] != nil {
		t.Fatalf("unexpected single callback: %#v", single)
	}
}

func TestOfficialSDKFlowForDirectAndGroupMessages(t *testing.T) {
	queue := make(chan sender.Job, 4)
	source := &scriptedMessages{batches: []wechat.PollResult{{
		Cursor: "cursor-1",
		Messages: []map[string]any{
			{
				"msgid": "101", "from": "wxid_friend", "roomid": "wxid_friend",
				"msgtime": int64(1_700_000_000_000), "msgtype": "text",
				"text": map[string]any{"content": "direct hello"},
			},
			{
				"msgid": "102", "from": "wxid_member", "roomid": "123@chatroom",
				"msgtime": int64(1_700_000_001_000), "msgtype": "text",
				"text": map[string]any{"content": "group hello"},
			},
		},
	}}}
	server := New("webox", "test-secret", source, queue, slog.Default())
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}

	writeFrame(t, connection, map[string]any{
		"cmd": subscribe, "headers": map[string]any{"req_id": "aibot_subscribe_1"},
		"body": map[string]any{
			"bot_id": "webox", "secret": "test-secret", "scene": 20, "plug_version": "test",
		},
	})
	assertAck(t, readFrame(t, connection), "aibot_subscribe_1")

	direct := readFrame(t, connection)
	directRequestID := frameRequestID(t, direct)
	directBody := frameBody(t, direct)
	if direct["cmd"] != messageCallback || directBody["chattype"] != "single" || directBody["chatid"] != nil {
		t.Fatalf("unexpected direct callback: %#v", direct)
	}
	group := readFrame(t, connection)
	groupRequestID := frameRequestID(t, group)
	groupBody := frameBody(t, group)
	if group["cmd"] != messageCallback || groupBody["chattype"] != "group" || groupBody["chatid"] != "123@chatroom" {
		t.Fatalf("unexpected group callback: %#v", group)
	}

	writeStream(t, connection, directRequestID, "stream-direct", "thinking", false)
	assertAck(t, readFrame(t, connection), directRequestID)
	writeStream(t, connection, directRequestID, "stream-direct", "direct reply", true)
	assertAck(t, readFrame(t, connection), directRequestID)
	assertJob(t, queue, sender.Job{Target: "wxid_friend", Content: "direct reply"})

	writeStream(t, connection, groupRequestID, "stream-group", "group reply", true)
	assertAck(t, readFrame(t, connection), groupRequestID)
	assertJob(t, queue, sender.Job{Target: "123@chatroom", Content: "group reply"})

	writeFrame(t, connection, map[string]any{
		"cmd": sendMessage, "headers": map[string]any{"req_id": "aibot_send_msg_1"},
		"body": map[string]any{
			"chatid": "123@chatroom", "msgtype": "markdown",
			"markdown": map[string]any{"content": "proactive group message"},
		},
	})
	assertAck(t, readFrame(t, connection), "aibot_send_msg_1")
	assertJob(t, queue, sender.Job{Target: "123@chatroom", Content: "proactive group message"})
}

func writeStream(t *testing.T, connection *websocket.Conn, requestID, streamID, content string, finish bool) {
	t.Helper()
	writeFrame(t, connection, map[string]any{
		"cmd": respondMessage, "headers": map[string]any{"req_id": requestID},
		"body": map[string]any{
			"msgtype": "stream",
			"stream":  map[string]any{"id": streamID, "content": content, "finish": finish},
		},
	})
}

func assertAck(t *testing.T, frame map[string]any, requestID string) {
	t.Helper()
	if integerValue(frame["errcode"]) != 0 || requestID != frameRequestID(t, frame) {
		t.Fatalf("unexpected ACK: %#v", frame)
	}
}

func assertJob(t *testing.T, queue <-chan sender.Job, expected sender.Job) {
	t.Helper()
	select {
	case job := <-queue:
		if job != expected {
			t.Fatalf("unexpected send job: %#v", job)
		}
	case <-time.After(time.Second):
		t.Fatal("send job was not queued")
	}
}

func frameRequestID(t *testing.T, frame map[string]any) string {
	t.Helper()
	headers, ok := frame["headers"].(map[string]any)
	if !ok {
		t.Fatalf("missing headers: %#v", frame)
	}
	requestID, _ := headers["req_id"].(string)
	if requestID == "" {
		t.Fatalf("missing request ID: %#v", frame)
	}
	return requestID
}

func frameBody(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	body, ok := frame["body"].(map[string]any)
	if !ok {
		t.Fatalf("missing body: %#v", frame)
	}
	return body
}

func writeFrame(t *testing.T, connection *websocket.Conn, value any) {
	t.Helper()
	if err := connection.WriteJSON(value); err != nil {
		t.Fatal(err)
	}
}

func readFrame(t *testing.T, connection *websocket.Conn) map[string]any {
	t.Helper()
	_, data, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

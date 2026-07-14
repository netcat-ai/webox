package wecom

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/netcat-ai/webox/internal/sender"
	"github.com/netcat-ai/webox/internal/wechat"
)

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

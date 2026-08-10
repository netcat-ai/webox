package runner_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netcat-ai/webox/tests/e2e/runner"
)

func TestDockerPeerDriverPassesTargetAndTextWithoutShellInterpolation(t *testing.T) {
	directory := t.TempDir()
	argumentsPath := filepath.Join(directory, "arguments")
	stdinPath := filepath.Join(directory, "stdin")
	dockerPath := filepath.Join(directory, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGS_PATH\"\ncat > \"$STDIN_PATH\"\n"
	if err := os.WriteFile(dockerPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGS_PATH", argumentsPath)
	t.Setenv("STDIN_PATH", stdinPath)
	driver, err := runner.NewDockerPeerDriver(runner.DockerPeerConfig{
		DockerBinary: dockerPath, Container: "webox-peer",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, text := "Webox私聊测试", "quote '$HOME' && $(false)"
	if err := driver.Send(context.Background(), target, text); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(arguments)), "\n")
	if len(lines) != 8 || strings.Join(lines[:6], " ") != "exec -i webox-peer bash -s --" {
		t.Fatalf("arguments=%q", lines)
	}
	decodedTarget, err := base64.StdEncoding.DecodeString(lines[6])
	if err != nil {
		t.Fatal(err)
	}
	decodedText, err := base64.StdEncoding.DecodeString(lines[7])
	if err != nil {
		t.Fatal(err)
	}
	if string(decodedTarget) != target || string(decodedText) != text {
		t.Fatalf("target=%q text=%q", decodedTarget, decodedText)
	}
	stdin, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stdin, []byte("xdotool")) || !bytes.Contains(stdin, []byte("xclip")) {
		t.Fatalf("peer script does not drive WeChat UI: %q", stdin)
	}
	if !bytes.Contains(stdin, []byte(`xdotool key --clearmodifiers Return`)) {
		t.Fatalf("peer script does not select the unique remark result with Enter: %q", stdin)
	}
	if bytes.Contains(stdin, []byte(`mousemove --window "$win" 150`)) {
		t.Fatalf("peer script still selects search results by fixed coordinates: %q", stdin)
	}
}

func TestDirectRoundTripCrossesBothILinkEndpoints(t *testing.T) {
	t.Helper()
	state := newRoundTripState()
	sut := httptest.NewServer(http.HandlerFunc(state.handleSUT))
	defer sut.Close()
	peer := httptest.NewServer(http.HandlerFunc(state.handlePeer))
	defer peer.Close()
	driver := &fakePeerDriver{state: state}

	testRunner, err := runner.New(runner.Config{
		SUTURL: sut.URL, PeerURL: peer.URL,
		SUTToken: "sut-token", PeerToken: "peer-token",
		PeerTarget: "Webox私聊测试", Timeout: 2 * time.Second,
	}, driver)
	if err != nil {
		t.Fatal(err)
	}
	result, err := testRunner.RunDirect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.RequestText, "WEBOX_E2E_") {
		t.Fatalf("request=%q", result.RequestText)
	}
	if result.ReplyText != "ACK_"+result.RequestText {
		t.Fatalf("reply=%q request=%q", result.ReplyText, result.RequestText)
	}
	if driver.target != "Webox私聊测试" || driver.text != result.RequestText {
		t.Fatalf("peer send target=%q text=%q", driver.target, driver.text)
	}
	if state.replyText != result.ReplyText || state.replyContext != "wxid-peer" {
		t.Fatalf("sut reply text=%q roomid=%q", state.replyText, state.replyContext)
	}
}

func TestDirectRoundTripRejectsPeerThatIsNotReady(t *testing.T) {
	state := newRoundTripState()
	state.peerReady = false
	sut := httptest.NewServer(http.HandlerFunc(state.handleSUT))
	defer sut.Close()
	peer := httptest.NewServer(http.HandlerFunc(state.handlePeer))
	defer peer.Close()
	driver := &fakePeerDriver{state: state}
	testRunner, err := runner.New(runner.Config{
		SUTURL: sut.URL, PeerURL: peer.URL,
		SUTToken: "sut-token", PeerToken: "peer-token",
		PeerTarget: "Webox私聊测试", Timeout: 2 * time.Second,
	}, driver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testRunner.RunDirect(context.Background()); err == nil || !strings.Contains(err.Error(), "peer is not ready") {
		t.Fatalf("error=%v", err)
	}
	if driver.target != "" {
		t.Fatalf("peer driver was called for target %q", driver.target)
	}
}

type fakePeerDriver struct {
	state  *roundTripState
	target string
	text   string
}

func (driver *fakePeerDriver) Send(_ context.Context, target, text string) error {
	driver.target, driver.text = target, text
	driver.state.mu.Lock()
	driver.state.requestText = text
	driver.state.mu.Unlock()
	close(driver.state.peerSent)
	return nil
}

type roundTripState struct {
	mu           sync.Mutex
	sutPolls     int
	peerPolls    int
	peerSent     chan struct{}
	sutReplied   chan struct{}
	replyText    string
	replyContext string
	requestText  string
	peerReady    bool
}

func newRoundTripState() *roundTripState {
	return &roundTripState{
		peerSent: make(chan struct{}), sutReplied: make(chan struct{}), peerReady: true,
	}
}

func (state *roundTripState) handleSUT(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/healthz" {
		writeJSON(response, map[string]any{"ok": true, "ready": true})
		return
	}
	if !validAuth(request, "sut-token") {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch request.URL.Path {
	case "/ilink/bot/getupdates":
		state.mu.Lock()
		state.sutPolls++
		poll := state.sutPolls
		state.mu.Unlock()
		if poll == 1 {
			writeJSON(response, map[string]any{"ret": 0, "msgs": []any{}, "get_updates_buf": "sut-baseline"})
			return
		}
		<-state.peerSent
		state.mu.Lock()
		requestText := state.requestText
		state.mu.Unlock()
		writeJSON(response, map[string]any{
			"ret": 0, "get_updates_buf": "sut-next",
			"msgs": []any{map[string]any{
				"msgid": "incoming-1", "action": "send", "from": "wxid-peer", "tolist": []string{},
				"roomid": "wxid-peer", "msgtime": 1781703356000, "msgtype": "text",
				"text": map[string]any{"content": requestText},
			}},
		})
	case "/ilink/bot/sendmessage":
		var body struct {
			Messages []struct {
				RoomID string `json:"roomid"`
				Text   struct {
					Content string `json:"content"`
				} `json:"text"`
			} `json:"msgs"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		state.replyText = body.Messages[0].Text.Content
		state.replyContext = body.Messages[0].RoomID
		close(state.sutReplied)
		writeJSON(response, map[string]any{"ret": 0, "client_msg_id": "outgoing-1"})
	default:
		http.NotFound(response, request)
	}
}

func (state *roundTripState) handlePeer(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/healthz" {
		writeJSON(response, map[string]any{"ok": true, "ready": state.peerReady})
		return
	}
	if !validAuth(request, "peer-token") {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	state.mu.Lock()
	state.peerPolls++
	poll := state.peerPolls
	state.mu.Unlock()
	if poll == 1 {
		writeJSON(response, map[string]any{"ret": 0, "msgs": []any{}, "get_updates_buf": "peer-baseline"})
		return
	}
	<-state.sutReplied
	writeJSON(response, map[string]any{
		"ret": 0, "get_updates_buf": "peer-next",
		"msgs": []any{map[string]any{
			"msgid": "reply-1", "action": "send", "from": "wxid-self", "tolist": []string{},
			"roomid": "wxid-self", "msgtime": 1781703356000, "msgtype": "text", "text": map[string]any{"content": state.replyText},
		}},
	})
}

func validAuth(request *http.Request, token string) bool {
	return request.Header.Get("AuthorizationType") == "ilink_bot_token" &&
		request.Header.Get("Authorization") == "Bearer "+token &&
		request.Header.Get("X-WECHAT-UIN") != ""
}

func writeJSON(response http.ResponseWriter, body any) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(body)
}

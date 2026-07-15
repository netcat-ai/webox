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
	if state.replyText != result.ReplyText || state.replyContext != "sut-context" {
		t.Fatalf("sut reply text=%q context=%q", state.replyText, state.replyContext)
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
				"msgid": "incoming-1", "text": requestText, "context_token": "sut-context",
			}},
		})
	case "/ilink/bot/sendmessage":
		var body struct {
			Message struct {
				Context string `json:"context_token"`
				Items   []struct {
					TextItem struct {
						Text string `json:"text"`
					} `json:"text_item"`
				} `json:"item_list"`
			} `json:"msg"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		state.replyText = body.Message.Items[0].TextItem.Text
		state.replyContext = body.Message.Context
		close(state.sutReplied)
		writeJSON(response, map[string]any{"ret": 0, "client_message_id": "outgoing-1"})
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
		"msgs": []any{map[string]any{"msgid": "reply-1", "text": state.replyText, "context_token": "peer-context"}},
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

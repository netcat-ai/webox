package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/netcat-ai/webox/internal/wechat"
)

func TestPostLoginEscapeRunsOncePerLoginSession(t *testing.T) {
	var state postLoginUIState
	if state.shouldDismiss(wechat.WaitingForLogin) || !state.shouldDismiss(wechat.Ready) {
		t.Fatal("unexpected first login state")
	}
	state.markDismissed()
	if state.shouldDismiss(wechat.Ready) {
		t.Fatal("dismissed twice")
	}
	state.shouldDismiss(wechat.WaitingForLogin)
	if !state.shouldDismiss(wechat.Ready) {
		t.Fatal("new login was not reset")
	}
}

func TestHealthDoesNotExposeInternalState(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	healthHandler(wechat.New(t.TempDir(), "test-token")).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status: %d", response.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true || body["ready"] != false || len(body) != 2 {
		t.Fatalf("unexpected body: %#v", body)
	}
}

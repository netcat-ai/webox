package main

import (
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

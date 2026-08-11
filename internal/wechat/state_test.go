package wechat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGeometryFindsMainWindow(t *testing.T) {
	geometry := "WINDOW=1\nWIDTH=280\nHEIGHT=380\nWINDOW=2\nWIDTH=980\nHEIGHT=710\n"
	if got := mainWindowFromGeometry(geometry); got != "2" {
		t.Fatalf("main window: %q", got)
	}
}

func TestKeyFileRejectsLegacyFields(t *testing.T) {
	state := New(t.TempDir())
	legacy, _ := json.Marshal(map[string]any{
		"version": 1, "wxid": "wxid_test", "dbDir": "/tmp/db", "keys": map[string]string{"a": "b"},
	})
	if err := os.WriteFile(filepath.Join(state.stateDir, "wechat.key"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := state.readKey(); err == nil {
		t.Fatal("legacy key file accepted")
	}
}

func TestMissingKeyMaterialInvalidatesReadyState(t *testing.T) {
	state := New(t.TempDir())
	state.initialized.Store(true)
	if _, _, err := state.readyDatabase(); err == nil {
		t.Fatal("missing key material accepted")
	}
	if state.IsInitialized() {
		t.Fatal("ready state was not invalidated")
	}
}

func TestInitializedStateToleratesTransientMissingMainWindow(t *testing.T) {
	state := New(t.TempDir())
	now := time.Unix(100, 0)
	if state.acceptMainWindowObservation(false, true, now) {
		t.Fatal("uninitialized state accepted a missing main window")
	}
	state.initialized.Store(true)
	if !state.acceptMainWindowObservation(false, true, now) ||
		!state.acceptMainWindowObservation(false, true, now.Add(mainWindowMissingGrace-time.Millisecond)) {
		t.Fatal("transient missing main window was not tolerated")
	}
	if state.acceptMainWindowObservation(false, true, now.Add(mainWindowMissingGrace)) {
		t.Fatal("persistently missing main window was accepted")
	}
	if !state.acceptMainWindowObservation(true, true, now.Add(mainWindowMissingGrace)) ||
		state.mainWindowMissingAt.Load() != 0 {
		t.Fatal("visible main window did not reset the grace period")
	}
	if state.acceptMainWindowObservation(false, false, now) {
		t.Fatal("unobservable main window was accepted")
	}
}

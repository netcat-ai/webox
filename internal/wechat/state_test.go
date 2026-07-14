package wechat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/netcat-ai/webox/internal/signedpayload"
	"github.com/netcat-ai/webox/internal/wechatdb"
)

func TestCursorRoundTripAndRejectLegacyFields(t *testing.T) {
	state := New(t.TempDir(), "test-token")
	cursor := dbCursor{
		StartedAt: 100,
		Positions: wechatdb.MessagePositions{"room": {"message/message_0.db": {CreateTime: 101, LocalID: 42}}},
	}
	encoded, err := state.encodeCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	var decoded dbCursor
	if err := signedpayload.Decode(state.cursorKey, encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Positions["room"]["message/message_0.db"].LocalID != 42 {
		t.Fatalf("unexpected cursor: %#v", decoded)
	}
	legacy, err := signedpayload.Encode(state.cursorKey, map[string]any{
		"v": 3, "source": "db", "started_at": 100, "positions": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := signedpayload.Decode(state.cursorKey, legacy, &decoded); err == nil {
		t.Fatal("legacy cursor accepted")
	}
}

func TestGeometrySeparatesLoginAndMainWindows(t *testing.T) {
	geometry := "WINDOW=1\nWIDTH=280\nHEIGHT=380\nWINDOW=2\nWIDTH=980\nHEIGHT=710\n"
	if got := loginWindowFromGeometry(geometry); got != "1" {
		t.Fatalf("login window: %q", got)
	}
	if got := mainWindowFromGeometry(geometry); got != "2" {
		t.Fatalf("main window: %q", got)
	}
}

func TestKeyFileRejectsLegacyFields(t *testing.T) {
	state := New(t.TempDir(), "test-token")
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
	state := New(t.TempDir(), "test-token")
	state.initialized.Store(true)
	if _, err := state.readyMaterial(); err == nil {
		t.Fatal("missing key material accepted")
	}
	if state.IsInitialized() {
		t.Fatal("ready state was not invalidated")
	}
}

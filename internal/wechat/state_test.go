package wechat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/netcat-ai/webox/internal/signedpayload"
	"github.com/netcat-ai/webox/internal/wechatdb"
)

func TestRemarkFilterAllowsOnlyWBConversationRemarks(t *testing.T) {
	messages := []map[string]any{
		{"roomid": "wxid-direct"},
		{"roomid": "family@chatroom"},
		{"roomid": "noise@chatroom"},
	}
	remarks := map[string]string{
		"wxid-direct":     "wb-alice",
		"family@chatroom": "wb-family",
		"noise@chatroom":  "other",
	}

	filtered, err := filterMessagesByRemarkPrefix(messages, func(roomID string) (string, error) {
		return remarks[roomID], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 2 {
		t.Fatalf("filtered=%#v", filtered)
	}
	got := []string{filtered[0]["roomid"].(string), filtered[1]["roomid"].(string)}
	if want := []string{"wxid-direct", "family@chatroom"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rooms=%v want=%v", got, want)
	}
}

func TestRemarkFilterSkipsMissingRoomAndCachesConversationRemark(t *testing.T) {
	messages := []map[string]any{
		{"roomid": "wxid-direct"},
		{"roomid": "wxid-direct"},
		{"text": map[string]any{"content": "missing room"}},
	}
	lookupCalls := 0

	filtered, err := filterMessagesByRemarkPrefix(messages, func(roomID string) (string, error) {
		lookupCalls++
		return "wb-direct", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 2 || lookupCalls != 1 {
		t.Fatalf("filtered=%#v lookupCalls=%d", filtered, lookupCalls)
	}
}

func TestDisabledRemarkFilterDoesNotReadRemarksOrDropMessages(t *testing.T) {
	state := New(t.TempDir(), "test-token", false)
	messages := []map[string]any{{"roomid": "unmarked"}}
	lookupCalled := false

	filtered, err := state.applyRemarkFilter(messages, func(string) (string, error) {
		lookupCalled = true
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if lookupCalled || len(filtered) != 1 {
		t.Fatalf("lookupCalled=%v filtered=%#v", lookupCalled, filtered)
	}
}

func TestCursorRoundTripAndRejectLegacyFields(t *testing.T) {
	state := New(t.TempDir(), "test-token", true)
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
	if err := state.ValidatePollCursor(encoded); err != nil {
		t.Fatalf("valid cursor rejected: %v", err)
	}
	if err := state.ValidatePollCursor(encoded + "x"); err == nil {
		t.Fatal("tampered cursor accepted")
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
	if err := state.ValidatePollCursor(legacy); err == nil {
		t.Fatal("legacy cursor validation succeeded")
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
	state := New(t.TempDir(), "test-token", true)
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
	state := New(t.TempDir(), "test-token", true)
	state.initialized.Store(true)
	if _, err := state.readyMaterial(); err == nil {
		t.Fatal("missing key material accepted")
	}
	if state.IsInitialized() {
		t.Fatal("ready state was not invalidated")
	}
}

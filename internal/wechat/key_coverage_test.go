package wechat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadKeyRejectsMissingMessageShardKey(t *testing.T) {
	state := New(t.TempDir(), "test-token")
	dbDir := t.TempDir()
	messageDir := filepath.Join(dbDir, "message")
	if err := os.MkdirAll(messageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(messageDir, "message_0.db"), []byte("encrypted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := state.writeKey(keyFile{
		WXID: "wxid_test", DBDir: dbDir,
		Keys: map[string]string{"session/session.db": strings.Repeat("1", 64)},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := state.readKey(); err == nil || !strings.Contains(err.Error(), "message/message_0.db") {
		t.Fatalf("missing message shard key was not rejected: %v", err)
	}
}

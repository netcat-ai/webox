package wechatdb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateMessageDBKeysRejectsMissingShardKey(t *testing.T) {
	dbDir := t.TempDir()
	messageDir := filepath.Join(dbDir, "message")
	if err := os.MkdirAll(messageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(messageDir, "message_0.db"), []byte("encrypted"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := ValidateMessageDBKeys(dbDir, map[string]string{
		"session/session.db": strings.Repeat("1", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "message/message_0.db") {
		t.Fatalf("missing message shard key was not reported: %v", err)
	}
}

func TestValidateMessageDBKeysAcceptsEveryShardKey(t *testing.T) {
	dbDir := t.TempDir()
	messageDir := filepath.Join(dbDir, "message")
	if err := os.MkdirAll(messageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"message_0.db", "message_1.db", "message_fts.db"} {
		if err := os.WriteFile(filepath.Join(messageDir, name), []byte("encrypted"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	err := ValidateMessageDBKeys(dbDir, map[string]string{
		"message/message_0.db": strings.Repeat("1", 64),
		"message/message_1.db": strings.Repeat("2", 64),
	})
	if err != nil {
		t.Fatalf("complete message shard keys rejected: %v", err)
	}
}

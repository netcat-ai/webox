package sharedmedia

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteInboxAndResolveOutbox(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("image")...)
	sharedPath, err := store.WriteInbox("room", "message", png, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sharedPath, "inbox/") {
		t.Fatalf("shared path = %q", sharedPath)
	}
	if contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(sharedPath))); err != nil || string(contents) != string(png) {
		t.Fatalf("contents=%q err=%v", contents, err)
	}
	outbox := filepath.Join(root, "outbox", "reply.png")
	if err := os.WriteFile(outbox, png, 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, contentType, err := store.ResolveOutbox("outbox/reply.png")
	resolvedInfo, resolvedErr := os.Stat(resolved)
	outboxInfo, outboxErr := os.Stat(outbox)
	if err != nil || resolvedErr != nil || outboxErr != nil || !os.SameFile(resolvedInfo, outboxInfo) || contentType != "image/png" {
		t.Fatalf("resolved=%q type=%q err=%v", resolved, contentType, err)
	}
}

func TestResolveOutboxRejectsEscapesAndNonImages(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(root, "outbox", "plain.txt")
	if err := os.WriteFile(plain, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret.png", "inbox/message.png", "outbox/plain.txt"} {
		if _, _, err := store.ResolveOutbox(path); err == nil {
			t.Fatalf("ResolveOutbox accepted %q", path)
		}
	}
}

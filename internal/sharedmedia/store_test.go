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
	resolved, contentType, err := store.ResolveImage("outbox/reply.png")
	resolvedInfo, resolvedErr := os.Stat(resolved)
	outboxInfo, outboxErr := os.Stat(outbox)
	if err != nil || resolvedErr != nil || outboxErr != nil || !os.SameFile(resolvedInfo, outboxInfo) || contentType != "image/png" {
		t.Fatalf("resolved=%q type=%q err=%v", resolved, contentType, err)
	}
}

func TestResolveImageAcceptsInboxAndRejectsEscapesAndNonImages(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(root, "outbox", "plain.txt")
	if err := os.WriteFile(plain, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(root, "inbox", "message.png")
	if err := os.WriteFile(inbox, append([]byte("\x89PNG\r\n\x1a\n"), []byte("image")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ResolveImage("inbox/message.png"); err != nil {
		t.Fatalf("resolve inbox image: %v", err)
	}
	for _, path := range []string{"../secret.png", "outbox/plain.txt"} {
		if _, _, err := store.ResolveImage(path); err == nil {
			t.Fatalf("ResolveImage accepted %q", path)
		}
	}
}

func TestCopyInboxAndResolveOutboxFile(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "源文件.txt")
	if err := os.WriteFile(source, []byte("contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	sharedPath, err := store.CopyInboxFile("room", "message", "源文件.txt", source)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sharedPath, "inbox/") || filepath.Base(sharedPath) != "源文件.txt" {
		t.Fatalf("shared path = %q", sharedPath)
	}
	if contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(sharedPath))); err != nil || string(contents) != "contents" {
		t.Fatalf("contents=%q err=%v", contents, err)
	}
	resolved, filename, err := store.ResolveFile(sharedPath)
	if err != nil || filename != "源文件.txt" {
		t.Fatalf("resolve inbox file: resolved=%q filename=%q err=%v", resolved, filename, err)
	}
	outbox := filepath.Join(root, "outbox", "report.pdf")
	if err := os.WriteFile(outbox, []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, filename, err = store.ResolveFile("outbox/report.pdf")
	resolvedInfo, resolvedErr := os.Stat(resolved)
	outboxInfo, outboxErr := os.Stat(outbox)
	if err != nil || resolvedErr != nil || outboxErr != nil || !os.SameFile(resolvedInfo, outboxInfo) || filename != "report.pdf" {
		t.Fatalf("resolved=%q filename=%q err=%v", resolved, filename, err)
	}
}

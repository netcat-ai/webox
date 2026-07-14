package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeListenAddr(t *testing.T) {
	if got := normalizeListenAddr(":8080"); got != "0.0.0.0:8080" {
		t.Fatalf("normalize port-only address: got %q", got)
	}
	if got := normalizeListenAddr("127.0.0.1:8080"); got != "127.0.0.1:8080" {
		t.Fatalf("preserve full address: got %q", got)
	}
}

func TestLoadOrCreateIDIsStable(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrCreateID(dir, "cursor-key", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateID(dir, "cursor-key", "")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 32 {
		t.Fatalf("ID is not stable: first=%q second=%q", first, second)
	}
	info, err := os.Stat(filepath.Join(dir, "cursor-key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected permissions: %o", info.Mode().Perm())
	}
	if strings.TrimSpace(first) == "" {
		t.Fatal("empty ID")
	}
}

func TestLoadCreatesStableILinkIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WEBOX_WEAGENT_STATE_DIR", dir)
	t.Setenv("WEBOX_PUBLIC_BASE_URL", " https://webox.example.test/base/ ")

	first, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	second, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if first.APIToken == "" || first.APIToken != second.APIToken {
		t.Fatalf("unstable API token: %q %q", first.APIToken, second.APIToken)
	}
	if !strings.HasPrefix(first.ProviderAccountID, "webox-") || first.ProviderAccountID != second.ProviderAccountID {
		t.Fatalf("unstable provider account ID: %q %q", first.ProviderAccountID, second.ProviderAccountID)
	}
	if first.PublicBaseURL != "https://webox.example.test/base" {
		t.Fatalf("public base URL=%q", first.PublicBaseURL)
	}
}

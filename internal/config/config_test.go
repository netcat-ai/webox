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
	first, err := loadOrCreateID(dir, "api-token", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateID(dir, "api-token", "")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 32 {
		t.Fatalf("ID is not stable: first=%q second=%q", first, second)
	}
	info, err := os.Stat(filepath.Join(dir, "api-token"))
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

func TestLoadCreatesStableILinkCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WEBOX_WEAGENT_STATE_DIR", dir)

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
	if first.SharedDir != filepath.Join(filepath.Dir(dir), "shared") {
		t.Fatalf("shared directory=%q", first.SharedDir)
	}
}

func TestLoadWeComArchive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WEBOX_WEAGENT_STATE_DIR", dir)
	t.Setenv("WEBOX_MESSAGE_SOURCE", MessageSourceWeComArchive)
	t.Setenv("WEBOX_WECOM_ARCHIVE_CORP_ID", "corp")
	t.Setenv("WEBOX_WECOM_ARCHIVE_SECRET", "secret")
	t.Setenv("WEBOX_WECOM_ARCHIVE_PRIVATE_KEY_FILE", "/run/secrets/private-key.pem")
	t.Setenv("WEBOX_WECOM_ARCHIVE_ACCOUNT_ID", "alice")
	t.Setenv("WEBOX_WECOM_ARCHIVE_WECHAT_ID", "archive-agent")
	t.Setenv("WEBOX_WECOM_ARCHIVE_INITIAL_SEQ", "42")

	configuration, err := LoadWeComProxy()
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Archive.CorpID != "corp" || configuration.Archive.AccountID != "alice" || configuration.Archive.InitialSeq != 42 {
		t.Fatalf("configuration=%+v", configuration)
	}
}

func TestWeAgentRejectsWeComSource(t *testing.T) {
	t.Setenv("WEBOX_WEAGENT_STATE_DIR", t.TempDir())
	t.Setenv("WEBOX_MESSAGE_SOURCE", MessageSourceWeComArchive)
	if _, err := Load(); err == nil {
		t.Fatal("weagent accepted the WeCom archive source")
	}
}

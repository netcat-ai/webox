package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	MessageSourceWeChat       = "wechat"
	MessageSourceWeComArchive = "wecom-archive"
)

type WeComArchive struct {
	CorpID         string
	Secret         string
	PrivateKeyFile string
	AccountID      string
	WeChatID       string
	Nickname       string
	InitialSeq     uint64
}

type Config struct {
	ListenAddr string
	APIToken   string
	StateDir   string
	SharedDir  string
}

type WeComProxyConfig struct {
	ListenAddr string
	APIToken   string
	StateDir   string
	SharedDir  string
	Archive    WeComArchive
}

func Load() (Config, error) {
	stateDir := envOr("WEBOX_WEAGENT_STATE_DIR", "/webox/state/weagent")
	apiToken, err := loadOrCreateID(stateDir, "api-token", "")
	if err != nil {
		return Config{}, err
	}
	if source := envOr("WEBOX_MESSAGE_SOURCE", MessageSourceWeChat); source != MessageSourceWeChat {
		return Config{}, fmt.Errorf("WEBOX_MESSAGE_SOURCE %q requires the wecomproxy binary", source)
	}
	return Config{
		ListenAddr: normalizeListenAddr(envOr("WEBOX_LISTEN_ADDR", "0.0.0.0:8080")),
		APIToken:   apiToken,
		StateDir:   stateDir,
		SharedDir:  envOr("WEBOX_SHARED_DIR", filepath.Join(filepath.Dir(stateDir), "shared")),
	}, nil
}

func LoadWeComProxy() (WeComProxyConfig, error) {
	stateDir := envOr("WEBOX_WEAGENT_STATE_DIR", "/webox/state/weagent")
	apiToken, err := loadOrCreateID(stateDir, "api-token", "")
	if err != nil {
		return WeComProxyConfig{}, err
	}
	archive, err := loadWeComArchive()
	if err != nil {
		return WeComProxyConfig{}, err
	}
	return WeComProxyConfig{
		ListenAddr: normalizeListenAddr(envOr("WEBOX_LISTEN_ADDR", "0.0.0.0:8080")),
		APIToken:   apiToken,
		StateDir:   stateDir,
		SharedDir:  envOr("WEBOX_SHARED_DIR", filepath.Join(filepath.Dir(stateDir), "shared")),
		Archive:    archive,
	}, nil
}

func loadWeComArchive() (WeComArchive, error) {
	archive := WeComArchive{
		CorpID:         os.Getenv("WEBOX_WECOM_ARCHIVE_CORP_ID"),
		Secret:         os.Getenv("WEBOX_WECOM_ARCHIVE_SECRET"),
		PrivateKeyFile: os.Getenv("WEBOX_WECOM_ARCHIVE_PRIVATE_KEY_FILE"),
		AccountID:      os.Getenv("WEBOX_WECOM_ARCHIVE_ACCOUNT_ID"),
		WeChatID:       os.Getenv("WEBOX_WECOM_ARCHIVE_WECHAT_ID"),
		Nickname:       os.Getenv("WEBOX_WECOM_ARCHIVE_NICKNAME"),
	}
	for name, value := range map[string]string{
		"WEBOX_WECOM_ARCHIVE_CORP_ID":          archive.CorpID,
		"WEBOX_WECOM_ARCHIVE_SECRET":           archive.Secret,
		"WEBOX_WECOM_ARCHIVE_PRIVATE_KEY_FILE": archive.PrivateKeyFile,
		"WEBOX_WECOM_ARCHIVE_ACCOUNT_ID":       archive.AccountID,
		"WEBOX_WECOM_ARCHIVE_WECHAT_ID":        archive.WeChatID,
	} {
		if value == "" {
			return WeComArchive{}, fmt.Errorf("%s is required", name)
		}
	}
	if value := os.Getenv("WEBOX_WECOM_ARCHIVE_INITIAL_SEQ"); value != "" {
		sequence, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return WeComArchive{}, fmt.Errorf("parse WEBOX_WECOM_ARCHIVE_INITIAL_SEQ: %w", err)
		}
		archive.InitialSeq = sequence
	}
	return archive, nil
}

func loadOrCreateID(stateDir, filename, prefix string) (string, error) {
	path := filepath.Join(stateDir, filename)
	if data, err := os.ReadFile(path); err == nil {
		if value := strings.TrimSpace(string(data)); value != "" {
			return value, nil
		}
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("create state directory %s: %w", stateDir, err)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate ID: %w", err)
	}
	value := prefix + hex.EncodeToString(random)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(value), 0o600); err != nil {
		return "", fmt.Errorf("write ID %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("persist ID %s: %w", path, err)
	}
	return value, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func normalizeListenAddr(value string) string {
	if strings.HasPrefix(value, ":") {
		return "0.0.0.0" + value
	}
	return value
}

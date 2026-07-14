package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	ListenAddr       string
	BotID            string
	BotSecret        string
	CursorKey        string
	QRScreenshotPath string
	StateDir         string
}

func Load() (Config, error) {
	stateDir := envOr("WEBOX_WEAGENT_STATE_DIR", "/webox/state/weagent")
	cursorKey, err := loadOrCreateID(stateDir, "cursor-key", "")
	if err != nil {
		return Config{}, err
	}
	return Config{
		ListenAddr:       normalizeListenAddr(envOr("WEBOX_LISTEN_ADDR", "0.0.0.0:8080")),
		BotID:            envOr("WEBOX_BOT_ID", "webox"),
		BotSecret:        envOr("WEBOX_BOT_SECRET", "webox-local-token"),
		CursorKey:        cursorKey,
		QRScreenshotPath: strings.TrimSpace(envOr("WEBOX_QR_SCREENSHOT_PATH", "/webox/runtime/xvfb/Xvfb_screen0")),
		StateDir:         stateDir,
	}, nil
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
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func normalizeListenAddr(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, ":") {
		return "0.0.0.0" + value
	}
	return value
}

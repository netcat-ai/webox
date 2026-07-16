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

type Config struct {
	ListenAddr          string
	APIToken            string
	ProviderAccountID   string
	PublicBaseURL       string
	CursorKey           string
	QRScreenshotPath    string
	StateDir            string
	RemarkFilterEnabled bool
}

func Load() (Config, error) {
	stateDir := envOr("WEBOX_WEAGENT_STATE_DIR", "/webox/state/weagent")
	remarkFilterEnabled, err := envBool("WEBOX_REMARK_FILTER_ENABLED", true)
	if err != nil {
		return Config{}, err
	}
	cursorKey, err := loadOrCreateID(stateDir, "cursor-key", "")
	if err != nil {
		return Config{}, err
	}
	apiToken, err := loadOrCreateID(stateDir, "api-token", "")
	if err != nil {
		return Config{}, err
	}
	providerAccountID, err := loadOrCreateID(stateDir, "provider-account-id", "webox-")
	if err != nil {
		return Config{}, err
	}
	return Config{
		ListenAddr:          normalizeListenAddr(envOr("WEBOX_LISTEN_ADDR", "0.0.0.0:8080")),
		APIToken:            apiToken,
		ProviderAccountID:   providerAccountID,
		PublicBaseURL:       strings.TrimRight(strings.TrimSpace(os.Getenv("WEBOX_PUBLIC_BASE_URL")), "/"),
		CursorKey:           cursorKey,
		QRScreenshotPath:    strings.TrimSpace(envOr("WEBOX_QR_SCREENSHOT_PATH", "/webox/runtime/xvfb/Xvfb_screen0")),
		StateDir:            stateDir,
		RemarkFilterEnabled: remarkFilterEnabled,
	}, nil
}

func envBool(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return value, nil
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

package sharedmedia

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Store struct {
	root string
}

func New(root string) (*Store, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("shared media directory is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve shared media directory: %w", err)
	}
	for _, name := range []string{"inbox", "outbox"} {
		if err := os.MkdirAll(filepath.Join(absolute, name), 0o700); err != nil {
			return nil, fmt.Errorf("create shared media directory: %w", err)
		}
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve shared media directory: %w", err)
	}
	return &Store{root: canonical}, nil
}

func (store *Store) WriteInbox(roomID, messageID string, data []byte, contentType string) (string, error) {
	extension := extensionFor(contentType)
	if len(data) == 0 || extension == "" {
		return "", errors.New("incoming image is empty or has an unsupported format")
	}
	roomHash := sha256.Sum256([]byte(strings.TrimSpace(roomID)))
	messageHash := sha256.Sum256([]byte(strings.TrimSpace(messageID)))
	relative := filepath.Join("inbox", hex.EncodeToString(roomHash[:8]), hex.EncodeToString(messageHash[:16])+extension)
	destination := filepath.Join(store.root, relative)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return "", fmt.Errorf("create incoming media directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".incoming-*")
	if err != nil {
		return "", fmt.Errorf("create incoming media file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("write incoming media file: %w", err)
	}
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("set incoming media permissions: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close incoming media file: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return "", fmt.Errorf("publish incoming media file: %w", err)
	}
	return filepath.ToSlash(relative), nil
}

func (store *Store) ResolveOutbox(sharedPath string) (string, string, error) {
	sharedPath = strings.TrimSpace(sharedPath)
	if sharedPath == "" || filepath.IsAbs(sharedPath) {
		return "", "", errors.New("image shared_path must be relative")
	}
	clean := filepath.Clean(filepath.FromSlash(sharedPath))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", errors.New("image shared_path escapes the shared media directory")
	}
	parts := strings.Split(filepath.ToSlash(clean), "/")
	if len(parts) < 2 || parts[0] != "outbox" {
		return "", "", errors.New("image shared_path must be under outbox/")
	}
	root, err := filepath.EvalSymlinks(store.root)
	if err != nil {
		return "", "", fmt.Errorf("resolve shared media root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(store.root, clean))
	if err != nil {
		return "", "", fmt.Errorf("resolve shared image: %w", err)
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", errors.New("image shared_path escapes the shared media directory")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", "", fmt.Errorf("inspect shared image: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", "", errors.New("shared image is not a regular file")
	}
	file, err := os.Open(resolved)
	if err != nil {
		return "", "", fmt.Errorf("open shared image: %w", err)
	}
	header := make([]byte, 512)
	read, readErr := io.ReadFull(file, header)
	_ = file.Close()
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return "", "", fmt.Errorf("read shared image: %w", readErr)
	}
	contentType := contentTypeFor(header[:read])
	if contentType == "" {
		return "", "", errors.New("shared file is not a supported image")
	}
	return resolved, contentType, nil
}

func contentTypeFor(data []byte) string {
	switch {
	case len(data) >= 3 && string(data[:3]) == "\xff\xd8\xff":
		return "image/jpeg"
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	default:
		return ""
	}
}

func extensionFor(contentType string) string {
	return map[string]string{
		"image/jpeg": ".jpg",
		"image/png":  ".png",
		"image/gif":  ".gif",
		"image/webp": ".webp",
	}[strings.TrimSpace(contentType)]
}

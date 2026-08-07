package wechatdb

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestFindImageDatsPrefersHighQualityThenStandardThenThumbnail(t *testing.T) {
	attachRoot := t.TempDir()
	roomID := "room@chatroom"
	imageMD5 := "0123456789abcdef0123456789abcdef"
	imageDir := filepath.Join(attachRoot, fmt.Sprintf("%x", md5.Sum([]byte(roomID))), "2026-08", "Img")
	if err := os.MkdirAll(imageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{
		"standard":  filepath.Join(imageDir, imageMD5+".dat"),
		"high":      filepath.Join(imageDir, imageMD5+"_h.dat"),
		"thumbnail": filepath.Join(imageDir, imageMD5+"_t.dat"),
	}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := findImageDats(attachRoot, roomID, imageMD5); len(got) != 3 || got[0] != paths["high"] || got[1] != paths["standard"] || got[2] != paths["thumbnail"] {
		t.Fatalf("all variants selected %#v, want high then standard then thumbnail", got)
	}
	if err := os.Remove(paths["high"]); err != nil {
		t.Fatal(err)
	}
	if got := findImageDats(attachRoot, roomID, imageMD5); len(got) != 2 || got[0] != paths["standard"] || got[1] != paths["thumbnail"] {
		t.Fatalf("fallback variants selected %#v, want standard then thumbnail", got)
	}
	if err := os.Remove(paths["standard"]); err != nil {
		t.Fatal(err)
	}
	if got := findImageDats(attachRoot, roomID, imageMD5); len(got) != 1 || got[0] != paths["thumbnail"] {
		t.Fatalf("thumbnail fallback selected %#v, want %q", got, paths["thumbnail"])
	}
}

func TestConvertWXGFRejectsInvalidHeader(t *testing.T) {
	if _, err := convertWXGFToJPEG([]byte("not a wxgf image")); err == nil {
		t.Fatal("non-WXGF input was accepted")
	}
}

func TestConvertWXGFFixture(t *testing.T) {
	path := os.Getenv("WEBOX_WXGF_TEST_FILE")
	if path == "" {
		t.Skip("WEBOX_WXGF_TEST_FILE is not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	jpeg, err := convertWXGFToJPEG(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(jpeg, []byte{0xff, 0xd8, 0xff}) {
		t.Fatalf("decoded output is not JPEG: %x", jpeg[:min(len(jpeg), 16)])
	}
	t.Logf("decoded %d-byte WXGF into %d-byte JPEG", len(data), len(jpeg))
}

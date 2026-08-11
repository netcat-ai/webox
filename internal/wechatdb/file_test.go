package wechatdb

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResourceStoredFilename(t *testing.T) {
	packed, err := hex.DecodeString("0A170A08647972642E7A6970120B647972642831292E7A6970")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := resourceStoredFilename(packed)
	if err != nil || stored != "dyrd(1).zip" {
		t.Fatalf("stored=%q err=%v", stored, err)
	}
	if _, err := resourceStoredFilename([]byte{0xff}); err == nil {
		t.Fatal("malformed packed_info was accepted")
	}
}

func TestLocalFilePathUsesDownloadedCollisionName(t *testing.T) {
	account := t.TempDir()
	created := time.Date(2026, 8, 4, 18, 0, 0, 0, time.Local)
	directory := filepath.Join(account, "msg", "file", "2026-08")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(directory, "report(1).pdf")
	if err := os.WriteFile(expected, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := localFilePath(account, created.Unix(), "report(1).pdf")
	if err != nil {
		t.Fatal(err)
	}
	gotInfo, gotErr := os.Stat(got)
	expectedInfo, expectedErr := os.Stat(expected)
	if gotErr != nil || expectedErr != nil || !os.SameFile(gotInfo, expectedInfo) {
		t.Fatalf("path=%q want=%q gotErr=%v expectedErr=%v", got, expected, gotErr, expectedErr)
	}
	if _, err := localFilePath(account, created.Unix(), "../secret"); err == nil {
		t.Fatal("escaping path was accepted")
	}
}

func TestSafeLocalFilenamePreservesSpaces(t *testing.T) {
	if got := safeLocalFilename(" report.pdf "); got != " report.pdf " {
		t.Fatalf("filename=%q", got)
	}
}

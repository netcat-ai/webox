package wechatdb

import (
	"bytes"
	"testing"
)

func TestWXGFHEVCPayload(t *testing.T) {
	payload := []byte{0, 0, 0, 1, 0x40, 0x01, 0xaa}
	data := append([]byte("wxgf private header"), payload...)
	if got := wxgfHEVCPayload(data); !bytes.Equal(got, payload) {
		t.Fatalf("payload=%x want=%x", got, payload)
	}
	if got := wxgfHEVCPayload(payload); got != nil {
		t.Fatalf("non-wxgf payload=%x", got)
	}
}

func TestConvertWXGFRejectsMissingHEVCPayload(t *testing.T) {
	if _, err := convertWXGFToJPEG([]byte("wxgf without video")); err == nil {
		t.Fatal("wxgf without HEVC payload was accepted")
	}
}

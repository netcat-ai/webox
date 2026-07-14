package qrsource

import (
	"encoding/binary"
	"image/color"
	"os"
	"path/filepath"
	"testing"

	qrcodegen "github.com/skip2/go-qrcode"
)

func TestReadsWechatQRFromXvfb(t *testing.T) {
	code, err := qrcodegen.New("https://login.weixin.qq.com/l/screen-test", qrcodegen.Medium)
	if err != nil {
		t.Fatal(err)
	}
	bitmap := code.Bitmap()
	data := xwdFixture(320, 240, func(x, y int) color.RGBA {
		qx, qy := (x-80)/5, (y-40)/5
		if x >= 80 && y >= 40 && qy < len(bitmap) && qx < len(bitmap[qy]) && bitmap[qy][qx] {
			return color.RGBA{R: 45, G: 65, B: 255, A: 255}
		}
		return color.RGBA{R: 255, G: 255, B: 255, A: 255}
	})
	path := filepath.Join(t.TempDir(), "screen.xwd")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := New(path).Latest()
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.LoginURL != "https://login.weixin.qq.com/l/screen-test" {
		t.Fatalf("unexpected QR result: %#v", result)
	}
}

func TestMissingFramebufferIsNotAnError(t *testing.T) {
	result, err := New(filepath.Join(t.TempDir(), "missing.xwd")).Latest()
	if err != nil || result != nil {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestScreenWithoutWechatQRIsIgnored(t *testing.T) {
	data := xwdFixture(80, 80, func(_, _ int) color.RGBA {
		return color.RGBA{R: 255, G: 255, B: 255, A: 255}
	})
	path := filepath.Join(t.TempDir(), "screen.xwd")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := New(path).Latest()
	if err != nil || result != nil {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func xwdFixture(width, height int, pixel func(int, int) color.RGBA) []byte {
	fields := []uint32{
		104, xwdFileVersion, xwdZPixmap, 24, uint32(width), uint32(height), 0,
		xwdMSBFirst, 32, xwdMSBFirst, 32, 32, uint32(width * 4), 4,
		0x00ff0000, 0x0000ff00, 0x000000ff, 8, 256, 0,
		uint32(width), uint32(height), 0, 0, 0,
	}
	out := make([]byte, 0, 104+width*height*4)
	for _, field := range fields {
		out = binary.BigEndian.AppendUint32(out, field)
	}
	out = append(out, 'X', 0, 0, 0)
	for y := range height {
		for x := range width {
			value := pixel(x, y)
			packed := uint32(value.R)<<16 | uint32(value.G)<<8 | uint32(value.B)
			out = binary.BigEndian.AppendUint32(out, packed)
		}
	}
	return out
}

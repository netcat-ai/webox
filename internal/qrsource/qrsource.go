package qrsource

import (
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"os"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

const (
	xwdFileVersion      = 7
	xwdZPixmap          = 2
	xwdMSBFirst         = 0
	xwdLSBFirst         = 1
	minScreenBluePixels = 1500
	minScreenQRSide     = 80
)

type Source struct {
	path string
}

type LoginCode struct {
	ID       string
	LoginURL string
}

type xwdHeader struct {
	headerSize    int
	fileVersion   uint32
	pixmapFormat  uint32
	width         uint32
	height        uint32
	byteOrder     uint32
	bitsPerPixel  uint32
	bytesPerLine  int
	redMask       uint32
	greenMask     uint32
	blueMask      uint32
	numberOfColor int
}

func New(path string) Source {
	return Source{path: path}
}

func (s Source) Latest() (*LoginCode, error) {
	if s.path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}
	screen, err := parseXWDScreen(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	if !looksLikeWechatLoginQR(screen) {
		return nil, nil
	}
	bitmap, err := gozxing.NewBinaryBitmapFromImage(screen)
	if err != nil {
		return nil, nil
	}
	result, err := qrcode.NewQRCodeReader().DecodeWithoutHints(bitmap)
	if err != nil || !hasMinimumBounds(result.GetResultPoints()) {
		return nil, nil
	}
	payload := result.GetText()
	if payload == "" {
		return nil, nil
	}
	return &LoginCode{
		ID:       fmt.Sprintf("xvfb-qr-%x", md5.Sum([]byte(payload))),
		LoginURL: payload,
	}, nil
}

func parseXWDScreen(data []byte) (*image.RGBA, error) {
	header, err := parseXWDHeader(data)
	if err != nil {
		return nil, err
	}
	if header.fileVersion != xwdFileVersion || header.pixmapFormat != xwdZPixmap {
		return nil, errors.New("unsupported xwd header")
	}
	if header.width == 0 || header.height == 0 {
		return nil, errors.New("empty xwd screen")
	}
	if header.bitsPerPixel != 24 && header.bitsPerPixel != 32 {
		return nil, fmt.Errorf("unsupported xwd bits_per_pixel %d", header.bitsPerPixel)
	}
	if header.byteOrder != xwdMSBFirst && header.byteOrder != xwdLSBFirst {
		return nil, fmt.Errorf("unsupported xwd byte_order %d", header.byteOrder)
	}
	bytesPerPixel := int(header.bitsPerPixel / 8)
	offset := header.headerSize + header.numberOfColor*12
	required := offset + header.bytesPerLine*int(header.height)
	if offset < 0 || required < offset || len(data) < required {
		return nil, errors.New("truncated xwd image")
	}
	out := image.NewRGBA(image.Rect(0, 0, int(header.width), int(header.height)))
	for y := range int(header.height) {
		row := data[offset+y*header.bytesPerLine : offset+(y+1)*header.bytesPerLine]
		for x := range int(header.width) {
			start := x * bytesPerPixel
			pixel := readXWDPixel(row[start:start+bytesPerPixel], header.byteOrder, header.bitsPerPixel)
			out.SetRGBA(x, y, color.RGBA{
				R: maskedChannel(pixel, header.redMask),
				G: maskedChannel(pixel, header.greenMask),
				B: maskedChannel(pixel, header.blueMask),
				A: 255,
			})
		}
	}
	return out, nil
}

func parseXWDHeader(data []byte) (xwdHeader, error) {
	if len(data) < 100 {
		return xwdHeader{}, errors.New("truncated xwd header")
	}
	if header, err := parseXWDHeaderEndian(data, binary.BigEndian); err == nil {
		return header, nil
	}
	return parseXWDHeaderEndian(data, binary.LittleEndian)
}

func parseXWDHeaderEndian(data []byte, order binary.ByteOrder) (xwdHeader, error) {
	field := func(index int) uint32 { return order.Uint32(data[index*4 : index*4+4]) }
	header := xwdHeader{
		headerSize:    int(field(0)),
		fileVersion:   field(1),
		pixmapFormat:  field(2),
		width:         field(4),
		height:        field(5),
		byteOrder:     field(7),
		bitsPerPixel:  field(11),
		bytesPerLine:  int(field(12)),
		redMask:       field(14),
		greenMask:     field(15),
		blueMask:      field(16),
		numberOfColor: int(field(19)),
	}
	if header.headerSize < 100 || header.headerSize > len(data) {
		return xwdHeader{}, errors.New("invalid xwd header size")
	}
	if header.fileVersion != xwdFileVersion {
		return xwdHeader{}, errors.New("invalid xwd version")
	}
	if header.width > 16384 || header.height > 16384 {
		return xwdHeader{}, errors.New("unreasonable xwd dimensions")
	}
	return header, nil
}

func readXWDPixel(data []byte, byteOrder, bitsPerPixel uint32) uint32 {
	if bitsPerPixel == 32 {
		if byteOrder == xwdMSBFirst {
			return binary.BigEndian.Uint32(data)
		}
		return binary.LittleEndian.Uint32(data)
	}
	var pixel uint32
	if byteOrder == xwdMSBFirst {
		for _, value := range data {
			pixel = pixel<<8 | uint32(value)
		}
		return pixel
	}
	for index := len(data) - 1; index >= 0; index-- {
		pixel = pixel<<8 | uint32(data[index])
	}
	return pixel
}

func maskedChannel(pixel, mask uint32) uint8 {
	if mask == 0 {
		return 0
	}
	shift := uint32(0)
	for mask&(1<<shift) == 0 {
		shift++
	}
	maximum := mask >> shift
	value := (pixel & mask) >> shift
	return uint8((value*255 + maximum/2) / maximum)
}

func looksLikeWechatLoginQR(screen *image.RGBA) bool {
	count := 0
	for y := screen.Bounds().Min.Y; y < screen.Bounds().Max.Y; y++ {
		for x := screen.Bounds().Min.X; x < screen.Bounds().Max.X; x++ {
			pixel := screen.RGBAAt(x, y)
			if isWechatQRBlue(pixel.R, pixel.G, pixel.B) {
				count++
				if count >= minScreenBluePixels {
					return true
				}
			}
		}
	}
	return false
}

func isWechatQRBlue(red, green, blue uint8) bool {
	return blue > 145 && red < 130 && green < 150 &&
		int(blue)-int(red) > 45 && int(blue)-int(green) > 35
}

func hasMinimumBounds(points []gozxing.ResultPoint) bool {
	if len(points) == 0 {
		return false
	}
	left, top := math.Inf(1), math.Inf(1)
	right, bottom := math.Inf(-1), math.Inf(-1)
	for _, point := range points {
		left = math.Min(left, point.GetX())
		right = math.Max(right, point.GetX())
		top = math.Min(top, point.GetY())
		bottom = math.Max(bottom, point.GetY())
	}
	return right-left >= minScreenQRSide && bottom-top >= minScreenQRSide
}

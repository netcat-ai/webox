//go:build !linux || !cgo

package wechatdb

import "errors"

func decodeWXGF([]byte) ([]byte, error) {
	return nil, errors.New("unsupported platform for WeChat WXGF decoder: Linux with CGO is required")
}

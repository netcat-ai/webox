//go:build !linux || !cgo

package wechatdb

import "errors"

func decodeWXGF([]byte) ([]byte, error) {
	return nil, errors.New("WeChat WXGF decoder requires Linux with CGO enabled")
}

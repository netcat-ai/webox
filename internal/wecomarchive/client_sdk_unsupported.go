//go:build !linux || !cgo

package wecomarchive

import "errors"

func OpenClient(_, _, _ string) (Client, error) {
	return nil, errors.New("WeCom archive requires Linux with cgo and a matching Finance SDK")
}

//go:build !linux || !amd64 || !cgo

package wecomarchive

import "errors"

func OpenClient(_, _, _ string) (Client, error) {
	return nil, errors.New("WeCom archive requires linux/amd64 with cgo")
}

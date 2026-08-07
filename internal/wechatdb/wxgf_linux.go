//go:build linux && cgo

package wechatdb

/*
#cgo LDFLAGS: -ldl

#include <dlfcn.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef int (*webox_wxam2pic_fn)(
	const uint8_t *, int32_t, uint8_t *, int32_t *, const void *
);

static void *webox_wxgf_codec_handle;
static webox_wxam2pic_fn webox_wxam2pic;
static char webox_wxgf_load_error[512];

static int webox_dlopen_global(const char *path) {
	if (dlopen(path, RTLD_NOW | RTLD_GLOBAL) != NULL) {
		return 0;
	}
	const char *message = dlerror();
	snprintf(
		webox_wxgf_load_error,
		sizeof(webox_wxgf_load_error),
		"load %s: %s",
		path,
		message == NULL ? "unknown dynamic loader error" : message
	);
	return -1;
}

static int webox_load_wxgf_decoder(const char *comm_path, const char *codec_path) {
	if (webox_wxam2pic != NULL) {
		return 0;
	}
	if (webox_dlopen_global("libz.so.1") != 0) {
		return -1;
	}
	if (webox_dlopen_global(comm_path) != 0) {
		return -1;
	}
	webox_wxgf_codec_handle = dlopen(codec_path, RTLD_NOW | RTLD_LOCAL);
	if (webox_wxgf_codec_handle == NULL) {
		const char *message = dlerror();
		snprintf(
			webox_wxgf_load_error,
			sizeof(webox_wxgf_load_error),
			"load %s: %s",
			codec_path,
			message == NULL ? "unknown dynamic loader error" : message
		);
		return -1;
	}
	dlerror();
	webox_wxam2pic = (webox_wxam2pic_fn)dlsym(webox_wxgf_codec_handle, "wxam_dec_wxam2pic_5");
	const char *message = dlerror();
	if (message != NULL || webox_wxam2pic == NULL) {
		snprintf(
			webox_wxgf_load_error,
			sizeof(webox_wxgf_load_error),
			"resolve wxam_dec_wxam2pic_5: %s",
			message == NULL ? "symbol not found" : message
		);
		return -1;
	}
	return 0;
}

static const char *webox_wxgf_decoder_error(void) {
	return webox_wxgf_load_error;
}

static int webox_decode_wxgf(
	const uint8_t *input,
	int32_t input_length,
	uint8_t *output,
	int32_t *output_length
) {
	uint64_t options[4];
	memset(options, 0, sizeof(options));
	return webox_wxam2pic(input, input_length, output, output_length, &options);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"
)

const (
	defaultWeChatLibraryDir = "/webox/wechat/opt/wechat"
	maxWXGFDecodedSize      = 512 << 20
	minWXGFOutputBuffer     = 1 << 20
	wxgfBufferTooSmall      = -206
)

var (
	wxgfDecoderOnce sync.Once
	wxgfDecoderErr  error
	wxgfDecoderMu   sync.Mutex
)

func decodeWXGF(data []byte) ([]byte, error) {
	if len(data) == 0 || len(data) > math.MaxInt32 {
		return nil, errors.New("WXGF input has an invalid size")
	}
	if err := loadWXGFDecoder(); err != nil {
		return nil, err
	}
	wxgfDecoderMu.Lock()
	defer wxgfDecoderMu.Unlock()

	capacity := len(data) * 4
	if capacity < minWXGFOutputBuffer {
		capacity = minWXGFOutputBuffer
	}
	if capacity > maxWXGFDecodedSize {
		capacity = maxWXGFDecodedSize
	}
	output := make([]byte, capacity)
	outputLength, result := callWXGFDecoder(data, output)
	if result == wxgfBufferTooSmall {
		if outputLength <= capacity || outputLength > maxWXGFDecodedSize {
			return nil, fmt.Errorf("WeChat WXGF decoder requested an invalid output size: %d", outputLength)
		}
		output = make([]byte, outputLength)
		outputLength, result = callWXGFDecoder(data, output)
	}
	if result != 0 {
		return nil, fmt.Errorf("WeChat WXGF decoder failed with code %d", result)
	}
	if outputLength <= 0 || outputLength > len(output) {
		return nil, fmt.Errorf("WeChat WXGF decoder returned an invalid output size: %d", outputLength)
	}
	return output[:outputLength], nil
}

func loadWXGFDecoder() error {
	wxgfDecoderOnce.Do(func() {
		libraryDir := defaultWeChatLibraryDir
		if wechatBinary := strings.TrimSpace(os.Getenv("WECHAT_BIN")); wechatBinary != "" {
			libraryDir = filepath.Dir(wechatBinary)
		}
		commPath := C.CString(filepath.Join(libraryDir, "libvoipComm.so"))
		codecPath := C.CString(filepath.Join(libraryDir, "libvoipCodec.so"))
		defer C.free(unsafe.Pointer(commPath))
		defer C.free(unsafe.Pointer(codecPath))
		if C.webox_load_wxgf_decoder(commPath, codecPath) != 0 {
			wxgfDecoderErr = fmt.Errorf("load WeChat WXGF decoder: %s", C.GoString(C.webox_wxgf_decoder_error()))
		}
	})
	return wxgfDecoderErr
}

func callWXGFDecoder(data, output []byte) (int, int) {
	outputLength := C.int32_t(len(output))
	result := C.webox_decode_wxgf(
		(*C.uint8_t)(unsafe.Pointer(&data[0])),
		C.int32_t(len(data)),
		(*C.uint8_t)(unsafe.Pointer(&output[0])),
		&outputLength,
	)
	return int(outputLength), int(result)
}

package main

import (
	"bytes"
	"context"
	"testing"
)

func TestHelpExitsSuccessfully(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

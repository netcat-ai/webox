package sender

import (
	"strings"
	"testing"
)

func TestShellQuoteSingle(t *testing.T) {
	if got := shellQuoteSingle("a'b"); got != `'a'"'"'b'` {
		t.Fatalf("unexpected quote: %s", got)
	}
}

func TestOpenChatUsesFirstSearchResultWithoutCoordinateFocus(t *testing.T) {
	script := openChatScript("query")
	if !strings.Contains(script, "set_clip 'query'; paste_clip; sleep 1.8") ||
		!strings.Contains(script, "key --clearmodifiers Return; sleep 1.5") {
		t.Fatalf("unexpected script: %s", script)
	}
	for _, forbidden := range []string{"Down", "getwindowgeometry", "mousemove", "click"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("script contains %q", forbidden)
		}
	}
}

func TestSendReturnsToContacts(t *testing.T) {
	script := sendTextScript("target", "message")
	if strings.LastIndex(script, "key --clearmodifiers Return") >= strings.LastIndex(script, "key --clearmodifiers ctrl+2") {
		t.Fatal("send does not return to contacts")
	}
}

func TestSendImageUsesClipboardWithoutAttachmentOrSendButtonClicks(t *testing.T) {
	script := sendImageScript("target", "/tmp/image.png", "image/png")
	for _, required := range []string{
		"xclip -selection clipboard -target 'image/png' -loops 5 -i '/tmp/image.png'",
		"paste_clip", "key --clearmodifiers Return", "key --clearmodifiers ctrl+2",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("script does not contain %q: %s", required, script)
		}
	}
	for _, forbidden := range []string{"ctrl+l", "file_x", "send_x", "getwindowgeometry", "mousemove", "click"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("script contains media-specific coordinate action %q", forbidden)
		}
	}
}

func TestSendFileUsesURIListClipboardWithoutCoordinates(t *testing.T) {
	script := sendFileScript("target", "ZmlsZTovLy90bXAvcmVwb3J0LnBkZg0K")
	for _, required := range []string{
		"xclip -selection clipboard -target text/uri-list -loops 5 -i",
		"paste_clip", "key --clearmodifiers Return", "key --clearmodifiers ctrl+2",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("script does not contain %q: %s", required, script)
		}
	}
	for _, forbidden := range []string{"ctrl+l", "file_x", "send_x", "getwindowgeometry", "mousemove", "click"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("script contains file coordinate action %q", forbidden)
		}
	}
}

func TestUIPreludeDoesNotDependOnWindowGeometry(t *testing.T) {
	script := strings.Join(uiScriptPrelude(), "; ")
	for _, forbidden := range []string{"getwindowgeometry", "mousemove", "click"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("script contains %q", forbidden)
		}
	}
}

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

func TestOpenChatUsesFirstSearchResult(t *testing.T) {
	script := openChatScript("query", false)
	if !strings.Contains(script, "set_clip 'query'; paste_clip; sleep 1.8") ||
		!strings.Contains(script, "key --clearmodifiers Return; sleep 1.5") {
		t.Fatalf("unexpected script: %s", script)
	}
	for _, forbidden := range []string{"Down", "mousemove", "click"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("script contains %q", forbidden)
		}
	}
}

func TestOpenFileHelperSelectsFunctionResult(t *testing.T) {
	script := openChatScript("query", true)
	if !strings.Contains(script, `mousemove --window "$win" 155 325`) ||
		!strings.Contains(script, "click --repeat 2 --delay 120 1") {
		t.Fatalf("unexpected script: %s", script)
	}
	if strings.Contains(script, "key --clearmodifiers Return; sleep 1.5") {
		t.Fatal("file helper uses the web search result")
	}
}

func TestSendReturnsToContacts(t *testing.T) {
	script := sendTextScript("target", "message", false)
	if strings.LastIndex(script, "key --clearmodifiers Return") >= strings.LastIndex(script, "key --clearmodifiers ctrl+2") {
		t.Fatal("send does not return to contacts")
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

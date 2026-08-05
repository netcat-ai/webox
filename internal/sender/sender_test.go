package sender

import (
	"strings"
	"testing"

	"github.com/netcat-ai/webox/internal/wechatdb"
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
	script := sendItemsScript("target", []preparedItem{{Kind: "text", Text: "message"}})
	if strings.LastIndex(script, "key --clearmodifiers Return") >= strings.LastIndex(script, "key --clearmodifiers ctrl+2") {
		t.Fatal("send does not return to contacts")
	}
}

func TestSendItemsStagesEverythingAndSendsOnce(t *testing.T) {
	script := sendItemsScript("target", []preparedItem{
		{Kind: "text", Text: "message"},
		{Kind: "image", Path: "/tmp/image.png", ContentType: "image/png"},
		{Kind: "file", Path: "/tmp/report.pdf", Filename: "report.pdf"},
	})
	for _, required := range []string{
		"set_clip 'bWVzc2FnZQ=='",
		"xclip -selection clipboard -target 'image/png' -loops 5 -i '/tmp/image.png'",
		"xclip -selection clipboard -target text/uri-list -loops 5 -i",
		"paste_clip", "key --clearmodifiers Return", "key --clearmodifiers ctrl+2",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("script does not contain %q: %s", required, script)
		}
	}
	if strings.Count(script, "xdotool key --clearmodifiers Return") != 2 {
		t.Fatalf("expected one search selection and one final send: %s", script)
	}
	if strings.Count(script, "xdotool key --clearmodifiers ctrl+2") != 1 {
		t.Fatalf("expected one return to contacts: %s", script)
	}
	for _, forbidden := range []string{"ctrl+l", "file_x", "send_x", "getwindowgeometry", "mousemove", "click"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("script contains media-specific coordinate action %q", forbidden)
		}
	}
}

func TestContainsItemsCountsDuplicates(t *testing.T) {
	expected := []preparedItem{
		{Kind: "image"},
		{Kind: "image"},
		{Kind: "file", Filename: "report.txt"},
		{Kind: "file", Filename: "report.txt"},
	}
	sent := []wechatdb.OutgoingItem{
		{Kind: "image"},
		{Kind: "file", Value: "report.txt"},
	}
	if containsItems(sent, expected) {
		t.Fatal("one outgoing item satisfied duplicate expected items")
	}
	sent = append(sent,
		wechatdb.OutgoingItem{Kind: "image"},
		wechatdb.OutgoingItem{Kind: "file", Value: "report.txt"},
	)
	if !containsItems(sent, expected) {
		t.Fatal("matching duplicate outgoing items were not found")
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

package sender

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/netcat-ai/webox/internal/sharedmedia"
	"github.com/netcat-ai/webox/internal/wechat"
	"github.com/netcat-ai/webox/internal/wechatdb"
)

const maxTextLength = 5000

type Service struct {
	wechat *wechat.State
	media  *sharedmedia.Store
}

type Receipt struct {
	ClientMessageID string
}

type Item struct {
	Kind       string
	Text       string
	SharedPath string
}

type preparedItem struct {
	Kind        string
	Text        string
	Path        string
	ContentType string
	Filename    string
}

func New(state *wechat.State, media *sharedmedia.Store) *Service {
	return &Service{wechat: state, media: media}
}

func (service *Service) Send(ctx context.Context, target string, items []Item) (Receipt, error) {
	target = strings.TrimSpace(target)
	if target == "" || len(target) > 200 {
		return Receipt{}, errors.New("recipient is empty or too long")
	}
	prepared, err := service.prepareItems(items)
	if err != nil {
		return Receipt{}, err
	}
	recipient, err := service.wechat.ResolveRecipient(target)
	if err != nil {
		return Receipt{}, err
	}
	positions, err := service.wechat.RoomMessagePositions(recipient.Username)
	if err != nil {
		return Receipt{}, err
	}
	receipt := Receipt{ClientMessageID: randomID()}
	if os.Getenv("WEBOX_UI_SEND_DRY_RUN") == "1" {
		return receipt, nil
	}
	search := base64.StdEncoding.EncodeToString([]byte(recipient.SearchTerm))
	if err := runUIScript(ctx, "80s", sendItemsScript(search, prepared), "send message"); err != nil {
		return Receipt{}, err
	}
	for range 20 {
		found, err := service.verifyItems(positions, recipient.Username, prepared)
		if err != nil {
			return Receipt{}, fmt.Errorf("verify sent message in WeChat db: %w", err)
		}
		if found {
			return receipt, nil
		}
		select {
		case <-ctx.Done():
			return Receipt{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return Receipt{}, errors.New("send verification failed: not every item was found in WeChat db")
}

func (service *Service) prepareItems(items []Item) ([]preparedItem, error) {
	if len(items) == 0 {
		return nil, errors.New("message items are empty")
	}
	prepared := make([]preparedItem, 0, len(items))
	for _, item := range items {
		switch item.Kind {
		case "text":
			text := strings.TrimSpace(item.Text)
			if text == "" || len(text) > maxTextLength {
				return nil, errors.New("text is empty or too long")
			}
			prepared = append(prepared, preparedItem{Kind: "text", Text: text})
		case "image":
			if service.media == nil {
				return nil, errors.New("shared media store is unavailable")
			}
			path, contentType, err := service.media.ResolveImage(item.SharedPath)
			if err != nil {
				return nil, err
			}
			prepared = append(prepared, preparedItem{Kind: "image", Path: path, ContentType: contentType})
		case "file":
			if service.media == nil {
				return nil, errors.New("shared media store is unavailable")
			}
			path, filename, err := service.media.ResolveFile(item.SharedPath)
			if err != nil {
				return nil, err
			}
			prepared = append(prepared, preparedItem{Kind: "file", Path: path, Filename: filename})
		default:
			return nil, fmt.Errorf("unsupported message item kind %q", item.Kind)
		}
	}
	return prepared, nil
}

func (service *Service) verifyItems(positions wechatdb.RoomMessagePositions, username string, items []preparedItem) (bool, error) {
	sent, err := service.wechat.OutgoingItemsAfter(positions, username)
	if err != nil {
		return false, err
	}
	return containsItems(sent, items), nil
}

func containsItems(sent []wechatdb.OutgoingItem, expected []preparedItem) bool {
	counts := make(map[wechatdb.OutgoingItem]int, len(sent))
	for _, item := range sent {
		counts[item]++
	}
	for _, item := range expected {
		key := wechatdb.OutgoingItem{Kind: item.Kind}
		if item.Kind == "text" {
			key.Value = item.Text
		} else if item.Kind == "file" {
			key.Value = item.Filename
		}
		if counts[key] == 0 {
			return false
		}
		counts[key]--
	}
	return true
}

func sendItemsScript(searchBase64 string, items []preparedItem) string {
	script := uiScriptPrelude()
	script = append(script, openChatScript(searchBase64))
	for _, item := range items {
		switch item.Kind {
		case "text":
			script = append(script,
				"set_clip "+shellQuoteSingle(base64.StdEncoding.EncodeToString([]byte(item.Text))),
				"paste_clip",
				"sleep 0.2",
			)
		case "image":
			script = append(script,
				"cleanup_clip",
				"xclip -selection clipboard -target "+shellQuoteSingle(item.ContentType)+" -loops 5 -i "+shellQuoteSingle(item.Path)+" >/dev/null 2>&1 & clip_pid=$!",
				"sleep 0.25",
				"paste_clip",
				"sleep 1",
			)
		case "file":
			fileURL := (&url.URL{Scheme: "file", Path: item.Path}).String() + "\r\n"
			script = append(script,
				"cleanup_clip",
				"printf '%s' "+shellQuoteSingle(base64.StdEncoding.EncodeToString([]byte(fileURL)))+" | base64 -d | xclip -selection clipboard -target text/uri-list -loops 5 -i >/dev/null 2>&1 & clip_pid=$!",
				"sleep 0.25",
				"paste_clip",
				"sleep 1",
			)
		}
	}
	script = append(script,
		"xdotool key --clearmodifiers Return",
		"sleep 0.7",
		"xdotool key --clearmodifiers ctrl+2",
		"sleep 0.2",
	)
	return strings.Join(script, "; ")
}

func uiScriptPrelude() []string {
	return []string{
		"set -e",
		`display="${DISPLAY:-}"`,
		`if [ -z "$display" ]; then for x in /tmp/.X11-unix/X*; do [ -e "$x" ] || continue; display=":${x##*X}"; break; done; fi`,
		`export DISPLAY="${display:-:1}"`,
		`command -v xclip >/dev/null 2>&1 || { echo "xclip not installed" >&2; exit 127; }`,
		`command -v xdotool >/dev/null 2>&1 || { echo "xdotool not installed" >&2; exit 127; }`,
		`clip_pid=""`,
		`cleanup_clip() { if [ -n "${clip_pid:-}" ]; then kill "$clip_pid" 2>/dev/null || true; wait "$clip_pid" 2>/dev/null || true; clip_pid=""; fi; }`,
		`set_clip() { cleanup_clip; printf '%s' "$1" | base64 -d | xclip -selection clipboard -target UTF8_STRING -loops 5 -i >/dev/null 2>&1 & clip_pid=$!; sleep 0.25; }`,
		`paste_clip() { xdotool key --clearmodifiers ctrl+v; for i in $(seq 1 30); do if ! kill -0 "$clip_pid" 2>/dev/null; then wait "$clip_pid" 2>/dev/null || true; clip_pid=""; sleep 0.1; return 0; fi; sleep 0.1; done; echo "wechat did not read clipboard" >&2; return 3; }`,
		`trap cleanup_clip EXIT`,
		`win="$(xdotool search --onlyvisible --class 'wechat' 2>/dev/null | tail -n1 || true)"`,
		`[ -n "$win" ] || { active="$(xdotool getactivewindow 2>/dev/null || true)"; active_name=""; if [ -n "$active" ]; then active_name="$(xdotool getwindowname "$active" 2>/dev/null || true)"; case "$active_name" in *微信*|*WeChat*) win="$active";; esac; fi; }`,
		`[ -n "$win" ] || win="$(xdotool search --onlyvisible --name '微信' 2>/dev/null | tail -n1 || true)"`,
		`[ -n "$win" ] || win="$(xdotool search --onlyvisible --name 'WeChat' 2>/dev/null | tail -n1 || true)"`,
		`[ -n "$win" ] || { echo "visible WeChat window not found" >&2; exit 2; }`,
		`xdotool windowactivate "$win"`,
		"sleep 0.2",
	}
}

func openChatScript(queryBase64 string) string {
	return fmt.Sprintf(
		`main_win="$(xdotool search --onlyvisible --class 'wechat' 2>/dev/null | tail -n1 || true)"; `+
			`if [ -n "$main_win" ]; then win="$main_win"; xdotool windowactivate "$win"; xdotool windowraise "$win" 2>/dev/null || true; sleep 0.2; fi; `+
			`xdotool key --clearmodifiers Escape; sleep 0.1; `+
			`xdotool key --clearmodifiers ctrl+f; sleep 0.3; `+
			`xdotool key --clearmodifiers ctrl+a BackSpace; sleep 0.2; `+
			`set_clip %s; paste_clip; sleep 1.8; `+
			`xdotool key --clearmodifiers Return; sleep 1.5`,
		shellQuoteSingle(queryBase64),
	)
}

func runUIScript(ctx context.Context, timeout, script, action string) error {
	output, err := exec.CommandContext(ctx, "timeout", timeout, "bash", "-lc", script).CombinedOutput()
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s failed: %w: %s", action, err, strings.TrimSpace(string(output)))
}

func shellQuoteSingle(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func randomID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return strconvFallbackID()
	}
	return hex.EncodeToString(value)
}

func strconvFallbackID() string {
	return fmt.Sprintf("%032x", time.Now().UnixNano())
}

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

func New(state *wechat.State, media *sharedmedia.Store) *Service {
	return &Service{wechat: state, media: media}
}

func (service *Service) SendText(ctx context.Context, target, text string) (Receipt, error) {
	if text == "" || len(text) > maxTextLength {
		return Receipt{}, errors.New("text is empty or too long")
	}
	return service.send(ctx, target, "text", "60s",
		func(search string) string {
			return sendTextScript(search, base64.StdEncoding.EncodeToString([]byte(text)))
		},
		func(positions wechatdb.RoomMessagePositions, username string) (bool, error) {
			return service.wechat.HasTextMessageAfter(positions, username, text)
		})
}

func (service *Service) SendImage(ctx context.Context, target, sharedPath string) (Receipt, error) {
	if service.media == nil {
		return Receipt{}, errors.New("shared media store is unavailable")
	}
	path, contentType, err := service.media.ResolveOutboxImage(sharedPath)
	if err != nil {
		return Receipt{}, err
	}
	return service.send(ctx, target, "image", "80s",
		func(search string) string { return sendImageScript(search, path, contentType) },
		func(positions wechatdb.RoomMessagePositions, username string) (bool, error) {
			return service.wechat.HasImageMessageAfter(positions, username)
		})
}

func (service *Service) SendFile(ctx context.Context, target, sharedPath string) (Receipt, error) {
	if service.media == nil {
		return Receipt{}, errors.New("shared media store is unavailable")
	}
	path, filename, err := service.media.ResolveFile(sharedPath)
	if err != nil {
		return Receipt{}, err
	}
	fileURL := (&url.URL{Scheme: "file", Path: path}).String() + "\r\n"
	return service.send(ctx, target, "file", "80s",
		func(search string) string {
			return sendFileScript(search, base64.StdEncoding.EncodeToString([]byte(fileURL)))
		},
		func(positions wechatdb.RoomMessagePositions, username string) (bool, error) {
			return service.wechat.HasFileMessageAfter(positions, username, filename)
		})
}

func (service *Service) send(
	ctx context.Context,
	target, kind, timeout string,
	script func(searchBase64 string) string,
	verify func(wechatdb.RoomMessagePositions, string) (bool, error),
) (Receipt, error) {
	target = strings.TrimSpace(target)
	if target == "" || len(target) > 200 {
		return Receipt{}, errors.New("recipient is empty or too long")
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
	if err := runUIScript(ctx, timeout, script(base64.StdEncoding.EncodeToString([]byte(recipient.SearchTerm))), "send "+kind); err != nil {
		return Receipt{}, err
	}
	for range 20 {
		found, err := verify(positions, recipient.Username)
		if err != nil {
			return Receipt{}, fmt.Errorf("verify sent %s in WeChat db: %w", kind, err)
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
	return Receipt{}, fmt.Errorf("send verification failed: %s was not found in WeChat db", kind)
}

func sendTextScript(searchBase64, textBase64 string) string {
	script := uiScriptPrelude()
	script = append(script,
		openChatScript(searchBase64),
		"set_clip "+shellQuoteSingle(textBase64),
		"paste_clip",
		"sleep 0.2",
		"xdotool key --clearmodifiers Return",
		"sleep 0.5",
		"xdotool key --clearmodifiers ctrl+2",
		"sleep 0.2",
	)
	return strings.Join(script, "; ")
}

func sendImageScript(searchBase64, imagePath, contentType string) string {
	return sendMediaScript(searchBase64,
		"xclip -selection clipboard -target "+shellQuoteSingle(contentType)+" -loops 5 -i "+shellQuoteSingle(imagePath)+" >/dev/null 2>&1 & clip_pid=$!")
}

func sendFileScript(searchBase64, fileURLBase64 string) string {
	return sendMediaScript(searchBase64,
		"printf '%s' "+shellQuoteSingle(fileURLBase64)+" | base64 -d | xclip -selection clipboard -target text/uri-list -loops 5 -i >/dev/null 2>&1 & clip_pid=$!")
}

func sendMediaScript(searchBase64, loadClipboard string) string {
	script := uiScriptPrelude()
	script = append(script,
		openChatScript(searchBase64),
		"cleanup_clip",
		loadClipboard,
		"sleep 0.25",
		"paste_clip",
		"sleep 1",
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

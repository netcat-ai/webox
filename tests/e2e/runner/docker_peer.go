package runner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type DockerPeerConfig struct {
	DockerBinary string
	Container    string
}

type dockerPeerDriver struct {
	binary    string
	container string
}

func NewDockerPeerDriver(config DockerPeerConfig) (PeerDriver, error) {
	binary := strings.TrimSpace(config.DockerBinary)
	if binary == "" {
		binary = "docker"
	}
	container := strings.TrimSpace(config.Container)
	if container == "" {
		return nil, errors.New("peer container is required")
	}
	return &dockerPeerDriver{binary: binary, container: container}, nil
}

func (driver *dockerPeerDriver) Send(ctx context.Context, target, text string) error {
	command := exec.CommandContext(
		ctx, driver.binary, "exec", "-i", driver.container, "bash", "-s", "--",
		base64.StdEncoding.EncodeToString([]byte(target)),
		base64.StdEncoding.EncodeToString([]byte(text)),
	)
	command.Stdin = strings.NewReader(peerSendScript)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("drive peer container %s: %w: %s", driver.container, err, strings.TrimSpace(string(output)))
	}
	return nil
}

const peerSendScript = `set -euo pipefail
target_b64="$1"
text_b64="$2"
display="${DISPLAY:-}"
if [ -z "$display" ]; then
  for socket in /tmp/.X11-unix/X*; do
    [ -e "$socket" ] || continue
    display=":${socket##*X}"
    break
  done
fi
export DISPLAY="${display:-:1}"
command -v xclip >/dev/null 2>&1
command -v xdotool >/dev/null 2>&1

clip_pid=""
cleanup_clip() {
  if [ -n "${clip_pid:-}" ]; then
    kill "$clip_pid" 2>/dev/null || true
    wait "$clip_pid" 2>/dev/null || true
    clip_pid=""
  fi
}
set_clip() {
  cleanup_clip
  printf '%s' "$1" | base64 -d | xclip -selection clipboard -target UTF8_STRING -loops 5 -i >/dev/null 2>&1 &
  clip_pid=$!
  sleep 0.25
}
paste_clip() {
  xdotool key --clearmodifiers ctrl+v
  for _ in $(seq 1 30); do
    if ! kill -0 "$clip_pid" 2>/dev/null; then
      wait "$clip_pid" 2>/dev/null || true
      clip_pid=""
      sleep 0.1
      return 0
    fi
    sleep 0.1
  done
  echo "peer WeChat did not read clipboard" >&2
  return 3
}
trap cleanup_clip EXIT

win="$(xdotool search --onlyvisible --class 'wechat' 2>/dev/null | tail -n1 || true)"
[ -n "$win" ] || win="$(xdotool search --onlyvisible --name '微信' 2>/dev/null | tail -n1 || true)"
[ -n "$win" ] || win="$(xdotool search --onlyvisible --name 'WeChat' 2>/dev/null | tail -n1 || true)"
[ -n "$win" ] || { echo "visible peer WeChat window not found" >&2; exit 2; }

xdotool windowactivate "$win"
xdotool windowraise "$win" 2>/dev/null || true
xdotool key --clearmodifiers Escape
sleep 0.1
xdotool key --clearmodifiers ctrl+f
sleep 0.3
xdotool key --clearmodifiers ctrl+a BackSpace
set_clip "$target_b64"
paste_clip
sleep 2.5
# Peer targets are unique contact/group remarks. The matching local result is
# therefore the first selectable result; Enter avoids brittle screen coordinates.
xdotool key --clearmodifiers Return
sleep 1.5
# Selecting a search result does not consistently transfer keyboard focus to the
# composer. Click a stable point inside the chat input area before pasting.
xdotool mousemove --window "$win" 640 620 click 1
sleep 0.2
set_clip "$text_b64"
paste_clip
sleep 0.2
xdotool key --clearmodifiers Return
sleep 0.5
`

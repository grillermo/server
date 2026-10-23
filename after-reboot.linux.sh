#!/usr/bin/env bash
# Linux boot script for the PC (Omarchy). The Linux counterpart of after-reboot.sh
# (which the macmini runs); both live in this shared, git-synced repo and each runs
# only on its own OS. Triggered at boot by after-reboot.service (systemd --user,
# with lingering enabled, so it starts without anyone logging in).
#
# Brings the PC's always-on servers up in detached tmux sessions, so each is
# attachable and its log is visible (`tmux attach -t <name>`):
#   - llama    : llama.cpp router, on private 127.0.0.1:8081
#   - mema     : Whisker voice-to-text server (CUDA), on private 127.0.0.1:8788
#   - gpu-gate : reverse proxy owning the public ports (8080 for llama, 8787 for
#                mema). It serializes the GPU: while one server answers a
#                GPU request the other gets a hardcoded "GPU not available" 503.
#                See gpu-gate/main.go.
#
#   pi   -> 127.0.0.1:8080  (gate) -> 127.0.0.1:8081  llama
#   phone-> <lan>:8787      (gate) -> 127.0.0.1:8788  mema
#
# Idempotent and self-healing: each service is (re)started only when it is not
# answering its health endpoint, so re-running this (or `systemctl --user restart
# after-reboot`) leaves a healthy service alone but revives a dead one, even when
# its tmux session is still lingering around it.
set -uo pipefail

# systemd --user gives a bare PATH; name every directory the servers need
# (/usr/bin has go, used to build the gate on first boot).
export PATH="$HOME/.local/bin:$HOME/src/llama.cpp/build/bin:/usr/local/bin:/usr/bin:/bin"

LLAMA_BIN="$HOME/src/llama.cpp/build/bin/llama-server"
LLAMA_ENV="$HOME/.config/llama-server/env"          # provides LLAMA_API_KEY
LLAMA_PRESETS="$HOME/.config/llama-server/models.ini"
GATE_DIR="$HOME/c/server/gpu-gate"
GATE_BIN="$GATE_DIR/gpu-gate"

# responding <url>: true when the endpoint answers at all (any HTTP code,
# including 401), false only when nothing is listening (curl code 000).
responding() { [ "$(curl -s -o /dev/null -m 2 -w '%{http_code}' "$1" 2>/dev/null)" != 000 ]; }

tmux start-server

# --- llama.cpp router (private 8081, behind the gate) ------------------------
# remain-on-exit keeps a crashed pane with its output; respawn-pane -k revives it.
if responding http://127.0.0.1:8081/health; then
  echo "after-reboot.linux: llama already serving, leaving it"
else
  echo "after-reboot.linux: starting llama"
  tmux has-session -t '=llama' 2>/dev/null || tmux new-session -d -s llama -x 200 -y 50
  tmux set-option -t '=llama:' remain-on-exit on >/dev/null
  tmux respawn-pane -k -t '=llama:' \
    "set -a; . '$LLAMA_ENV'; set +a; exec '$LLAMA_BIN' --models-preset '$LLAMA_PRESETS' --host 127.0.0.1 --port 8081 --models-max 1 --api-key \"\$LLAMA_API_KEY\""
fi

# --- Whisker (mema) voice-to-text server (private 8788, behind the gate) -----
# ./serve sources ~/c/server/serve-in-place.sh (tmux session "mema"), binds
# loopback 8788 and advertises the gate's 8787 over mDNS; serve_attach no-ops
# without a tty. It is the same entry point git-sync's ./activate runs.
if responding http://127.0.0.1:8788/v1/health; then
  echo "after-reboot.linux: mema (whisker) already serving, leaving it"
else
  echo "after-reboot.linux: starting mema (whisker)"
  ( cd "$HOME/c/mema" && ./serve ) || echo "after-reboot.linux: mema ./serve returned non-zero"
fi

# --- gpu-gate (public 8080 + 8787) ------------------------------------------
# Build it once if the binary is missing (go is in /usr/bin); a plain Go stdlib
# program, so the build is quick and has no network dependency.
if responding http://127.0.0.1:8080/gpu-gate/health; then
  echo "after-reboot.linux: gpu-gate already serving, leaving it"
else
  if [ ! -x "$GATE_BIN" ]; then
    echo "after-reboot.linux: building gpu-gate"
    ( cd "$GATE_DIR" && go build -o gpu-gate . ) || echo "after-reboot.linux: gpu-gate build failed"
  fi
  echo "after-reboot.linux: starting gpu-gate"
  tmux has-session -t '=gpu-gate' 2>/dev/null || tmux new-session -d -s gpu-gate -x 200 -y 50
  tmux set-option -t '=gpu-gate:' remain-on-exit on >/dev/null
  tmux respawn-pane -k -t '=gpu-gate:' "exec '$GATE_BIN'"
fi

echo "after-reboot.linux: sessions -> $(tmux ls -F '#{session_name}' 2>/dev/null | tr '\n' ' ')"

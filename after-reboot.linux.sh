#!/usr/bin/env bash
# Linux boot script for the PC (Omarchy). The Linux counterpart of after-reboot.sh
# (which the macmini runs); both live in this shared, git-synced repo and each runs
# only on its own OS. Triggered at boot by after-reboot.service (systemd --user,
# with lingering enabled, so it starts without anyone logging in).
#
# Brings the GPU setup up in one detached tmux session, "gpu", with a titled
# pane per process, stacked top to bottom (`tmux attach -t gpu`):
#   - gate  : gpu-gate, the reverse proxy owning the public ports (8080 for
#             llama, 8787 for mema). It serializes the GPU: while one server
#             answers a GPU request the others get a hardcoded "GPU not
#             available" 503. It also unloads the idle servers' models when
#             another one needs the GPU (llama's and tts's before a
#             transcription, and so on). See gpu-gate/main.go. It also
#             hands simple-enhance a GPU lease per job on 127.0.0.1:8792.
#   - llama : llama.cpp router, on private 127.0.0.1:8081
#   - mema  : Whisker voice-to-text server (CUDA), on private 127.0.0.1:8788,
#             started by ~/c/mema/serve (which git-sync also runs on new code)
#   - tts   : text-to-speech server (Qwen3-TTS, CUDA), on private
#             127.0.0.1:8791, started by ~/c/server/tts/serve. It loads its
#             model on the first narration, so it takes no VRAM until used.
#   - enhance: simple-enhance (~/c/simple-enhance), video audio cleanup, on
#             0.0.0.0:7000. Its models load with the first job, under a
#             lease from the gate.
#
# gpu-gate/serve runs this and then attaches the terminal to the session.
#
#   pi   -> 127.0.0.1:8080  (gate) -> 127.0.0.1:8081  llama
#   phone-> <lan>:8787      (gate) -> 127.0.0.1:8788  mema
#   mac  -> <lan>:8790      (gate) -> 127.0.0.1:8791  tts
#
# Idempotent and self-healing: each process is (re)started only when it is not
# answering its health endpoint, so re-running this (or `systemctl --user restart
# after-reboot`) leaves a healthy one alone but revives a dead one in its pane.
# Any other pane in the session (a bare shell tmux-resurrect brought back) is
# closed.
set -uo pipefail

# systemd --user gives a bare PATH; name every directory the servers need
# (/usr/bin has go, used to build the gate on first boot).
export PATH="$HOME/.local/bin:$HOME/src/llama.cpp/build/bin:/usr/local/bin:/usr/bin:/bin"

LLAMA_BIN="$HOME/src/llama.cpp/build/bin/llama-server"
LLAMA_ENV="$HOME/.config/llama-server/env"          # provides LLAMA_API_KEY
LLAMA_PRESETS="$HOME/.config/llama-server/models.ini"
MEMA_ENV="$HOME/c/mema/server/.env"                  # provides WHISKER_AUTH_TOKEN
TTS_ENV="$HOME/c/server/tts/.env"                    # provides TTS_AUTH_TOKEN
ENHANCE_ENV="$HOME/c/simple-enhance/.env"            # provides ENHANCE_AUTH_TOKEN
GATE_DIR="$HOME/c/server/gpu-gate"
GATE_BIN="$GATE_DIR/gpu-gate"

# start_pane/pane_for from the helper every ./serve uses, so ~/c/mema/serve
# lands its pane in this same session.
SESSION=gpu
APP_DIR="$HOME"
# shellcheck source=serve-in-place.sh
. "$HOME/c/server/serve-in-place.sh"

# responding <url>: true when the endpoint answers at all (any HTTP code,
# including 401), false only when nothing is listening (curl code 000).
responding() { [ "$(curl -s -o /dev/null -m 2 -w '%{http_code}' "$1" 2>/dev/null)" != 000 ]; }

tmux start-server
if ! tmux has-session -t "=$SESSION" 2>/dev/null; then
  tmux new-session -d -s "$SESSION" -n main -x 200 -y 60
fi
tmux set-option -t "=$SESSION:" @serve-layout even-vertical
tmux set-option -w -t "=$SESSION:" pane-border-status top
tmux set-option -w -t "=$SESSION:" pane-border-format ' #{pane_title} '
tmux set-option -w -t "=$SESSION:" allow-set-title off # keep the titles ours

# --- gpu-gate (public 8080 + 8787 + 8790, lease 8792) ------------------------------------
# Build it when the binary is missing or older than its sources (go is in
# /usr/bin); a plain Go stdlib program, so the build is quick and has no
# network dependency.
if responding http://127.0.0.1:8080/gpu-gate/health; then
  echo "after-reboot.linux: gpu-gate already serving, leaving it"
else
  if [ ! -x "$GATE_BIN" ] || [ -n "$(find "$GATE_DIR" -name '*.go' -newer "$GATE_BIN")" ]; then
    echo "after-reboot.linux: building gpu-gate"
    ( cd "$GATE_DIR" && go build -o gpu-gate . ) || echo "after-reboot.linux: gpu-gate build failed"
  fi
  echo "after-reboot.linux: starting gpu-gate"
  # The servers' keys let the gate unload one's model before another takes
  # the GPU. Sourced whole; the gate reads only the four it names.
  APP_DIR="$GATE_DIR" SERVICE=gpu-gate start_pane gate \
    "bash -c 'set -a; . \"$LLAMA_ENV\"; . \"$MEMA_ENV\"; . \"$TTS_ENV\"; . \"$ENHANCE_ENV\"; set +a; exec \"$GATE_BIN\"'"
fi

# --- llama.cpp router (private 8081, behind the gate) ------------------------
if responding http://127.0.0.1:8081/health; then
  echo "after-reboot.linux: llama already serving, leaving it"
else
  echo "after-reboot.linux: starting llama"
  SERVICE=llama start_pane llama \
    "bash -c 'set -a; . \"$LLAMA_ENV\"; set +a; exec \"$LLAMA_BIN\" --models-preset \"$LLAMA_PRESETS\" --host 127.0.0.1 --port 8081 --models-max 1 --api-key \"\$LLAMA_API_KEY\"'"
fi

# --- Whisker (mema) voice-to-text server (private 8788, behind the gate) -----
# ./serve binds loopback 8788, advertises the gate's 8787 over mDNS and adds the
# "mema" pane to this session; serve_attach no-ops without a tty. It is the same
# entry point git-sync's ./activate runs.
if responding http://127.0.0.1:8788/v1/health; then
  echo "after-reboot.linux: mema (whisker) already serving, leaving it"
else
  echo "after-reboot.linux: starting mema (whisker)"
  ( cd "$HOME/c/mema" && ./serve ) || echo "after-reboot.linux: mema ./serve returned non-zero"
fi

# --- TTS server (private 8791, behind the gate) ------------------------------
# ./serve adds the "tts" pane to this session; it needs the venv and .env that
# its header describes, so a machine without them just logs the failure.
if responding http://127.0.0.1:8791/v1/health; then
  echo "after-reboot.linux: tts already serving, leaving it"
else
  echo "after-reboot.linux: starting tts"
  ( cd "$HOME/c/server/tts" && ./serve ) || echo "after-reboot.linux: tts ./serve returned non-zero"
fi

# --- simple-enhance (7000; GPU lease from the gate's 8792) --------------------
# Its models load with the first job, under a lease, so it takes no VRAM while
# idle; GPU_GATE_URL is set here too so it never loads them at startup. The Go
# server reads the rest of its settings from its own .env.
ENHANCE_DIR="$HOME/c/simple-enhance"
if responding http://127.0.0.1:7000/; then
  echo "after-reboot.linux: simple-enhance already serving, leaving it"
elif [ ! -x "$ENHANCE_DIR/.venvs/reuse/bin/python" ] || [ ! -f "$ENHANCE_DIR/.env" ]; then
  echo "after-reboot.linux: simple-enhance not set up (needs .venvs/reuse and .env), skipping"
else
  echo "after-reboot.linux: starting simple-enhance"
  APP_DIR="$ENHANCE_DIR" SERVICE=simple-enhance start_pane enhance \
    "env GPU_GATE_URL=http://127.0.0.1:8792 go run ./cmd/server"
fi

# Close whatever else is in the session (resurrected shells, the first pane of
# a new session), and kill the per-server sessions this replaced.
tmux list-panes -t "=$SESSION:" -F '#{pane_id} #{?#{@serve-title},#{@serve-title},#{pane_title}}' |
  awk '$2!="gate" && $2!="llama" && $2!="mema" && $2!="tts" && $2!="enhance" {print $1}' |
  while read -r pane; do tmux kill-pane -t "$pane"; done
tmux select-layout -t "=$SESSION:" even-vertical >/dev/null
for old in llama mema gpu-gate; do
  tmux kill-session -t "=$old" 2>/dev/null && echo "after-reboot.linux: killed old session $old"
done

echo "after-reboot.linux: panes in $SESSION -> $(tmux list-panes -t "=$SESSION:" -F '#{pane_title}' | tr '\n' ' ')"

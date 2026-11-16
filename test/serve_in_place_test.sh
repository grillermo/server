#!/usr/bin/env bash
# bash test/serve_in_place_test.sh - exercises serve-in-place.sh against a
# throwaway service: its own tmux server (-L), its own temp APP_DIR and a
# python3 http.server standing in for Puma. It never touches a real session,
# port or service. Set LIB to test another copy of the library.
#
# The regressions it pins, both seen on the macmini on 2026-11-16, where
# git-sync's ./activate left bank-app-parser dead instead of restarting it:
#   - free_port returned at once whenever nothing listened on the port, while
#     the old server was alive and still owned its pidfile, so the new one quit
#     with "A server is already running".
#   - remain-on-exit was set on the session's current window instead of the
#     server pane's, so a pane whose boot failed closed and took its output
#     with it; wait_up then polled a destroyed pane for the full 60s and
#     reported "(no web pane to inspect)".
set -uo pipefail

REAL_TMUX=$(command -v tmux) || { echo "SKIP: no tmux"; exit 0; }
command -v python3 >/dev/null || { echo "SKIP: no python3"; exit 0; }

LIB="${LIB:-$(cd "$(dirname "$0")/.." && pwd)/serve-in-place.sh}"
SOCK=serve-test-$$
TMUX="$REAL_TMUX -L $SOCK"
APP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/serve-in-place-test.XXXXXX")
SESSION=serve-test
PORT=${PORT:-47311}
pass=0; fail=0

ok()   { pass=$((pass+1)); printf 'PASS  %s\n' "$1"; }
bad()  { fail=$((fail+1)); printf 'FAIL  %s\n     %s\n' "$1" "${2:-}"; }
check() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "expected '$3', got '$2'"; fi; }

cleanup() {
  $TMUX kill-server 2>/dev/null
  pkill -f "serve-test-fake-$$" 2>/dev/null
  rm -rf "$APP_DIR"
}
trap cleanup EXIT

mkdir -p "$APP_DIR/tmp/pids"
cd "$APP_DIR" || exit 1

# tmux.conf's hook and default, reproduced on our own tmux server: this is the
# behavior the fix has to survive.
cat >"$APP_DIR/tmux.conf" <<'EOF'
set -g remain-on-exit off
set-hook -gw pane-died 'run-shell "[ #{||:#{pane_dead_signal},#{||:#{==:#{pane_dead_status},0},#{||:#{==:#{pane_dead_status},130},#{==:#{pane_dead_status},143}}}} = 1 ] && tmux kill-pane -t #{pane_id}"'
EOF
$TMUX -f "$APP_DIR/tmux.conf" start-server

# The library calls bare `tmux`; point that at our private server.
BIN=$APP_DIR/bin
mkdir -p "$BIN"
printf '#!/bin/sh\nexec %s -L %s "$@"\n' "$REAL_TMUX" "$SOCK" >"$BIN/tmux"
chmod +x "$BIN/tmux"
PATH="$BIN:$PATH"

export SESSION APP_DIR
# shellcheck source=/dev/null
. "$LIB"
# The library prepends its own PATH when sourced, which shadows our wrapper and
# would point every tmux call at the real server. Put the wrapper back in front.
PATH="$BIN:$PATH"
export PATH

echo "== APP_DIR=$APP_DIR  port=$PORT  tmux socket=$SOCK"
echo

### Unit: pidfile_pids -------------------------------------------------------
rm -f "$APP_DIR/tmp/pids/server.pid"
check "pidfile_pids: no file -> empty" "$(pidfile_pids)" ""

: >"$APP_DIR/tmp/pids/server.pid"
check "pidfile_pids: empty file -> empty" "$(pidfile_pids)" ""

echo 999999 >"$APP_DIR/tmp/pids/server.pid"
check "pidfile_pids: dead pid -> empty" "$(pidfile_pids)" ""

echo $$ >"$APP_DIR/tmp/pids/server.pid"
check "pidfile_pids: live pid -> that pid" "$(pidfile_pids)" "$$"
rm -f "$APP_DIR/tmp/pids/server.pid"

### Unit: pane_gone ----------------------------------------------------------
check "pane_gone: bogus pane id -> true" "$(pane_gone %99999 && echo gone || echo here)" "gone"

### free_port: THE 07:28/07:34/07:42 BUG -------------------------------------
# A process that owns the pidfile but no longer listens: the state every
# failed run was in. Before the fix free_port returned instantly and the new
# server quit with "A server is already running".
cat >"$APP_DIR/draining.sh" <<'EOF'
#!/usr/bin/env bash
# serve-test-fake: a Puma that has closed its listener and is still draining.
echo $$ > tmp/pids/server.pid
trap 'rm -f tmp/pids/server.pid; exit 0' TERM
sleep 300 &
wait
EOF
chmod +x "$APP_DIR/draining.sh"
( exec -a "serve-test-fake-$$-draining" "$APP_DIR/draining.sh" ) &
for _ in $(seq 1 40); do [ -s "$APP_DIR/tmp/pids/server.pid" ] && break; sleep 0.1; done
drainer=$(cat "$APP_DIR/tmp/pids/server.pid")

check "free_port: sees the pidfile owner with no listener" "$(port_owners "$PORT")" "$drainer"

start=$(date +%s)
free_port "$PORT" >/dev/null 2>&1
rc=$?
took=$(($(date +%s) - start))
check "free_port: succeeds against a draining server" "$rc" "0"
if kill -0 "$drainer" 2>/dev/null; then
  bad "free_port: waits for the old process to exit" "pid $drainer still alive after free_port returned"
else
  ok "free_port: waits for the old process to exit (took ${took}s)"
fi

### free_port: nothing to free ----------------------------------------------
rm -f "$APP_DIR/tmp/pids/server.pid"
start=$(date +%s)
free_port "$PORT" >/dev/null 2>&1
rc=$?
check "free_port: no owners -> instant success" "$rc" "0"
check "free_port: no owners -> did not sleep" "$(( $(date +%s) - start < 2 ))" "1"

### serve_pane: a healthy boot ----------------------------------------------
cat >"$APP_DIR/good.sh" <<'EOF'
#!/usr/bin/env bash
echo $$ > tmp/pids/server.pid
exec python3 -m http.server "$1" --bind 127.0.0.1
EOF
chmod +x "$APP_DIR/good.sh"

serve_pane web "$PORT" "$APP_DIR/good.sh $PORT" / >"$APP_DIR/boot1.log" 2>&1
rc=$?
check "serve_pane: healthy server reports up" "$rc" "0"
webpane=$(pane_for web)
if [ -n "$webpane" ]; then ok "serve_pane: web pane exists ($webpane)"; else bad "serve_pane: web pane exists" "pane_for found nothing"; fi

### FINDING 2: remain-on-exit lands on the server's own pane ----------------
check "remain-on-exit: set on the server pane" \
  "$($TMUX show-options -p -t "$webpane" remain-on-exit | awk '{print $2}')" "on"

# Make the session's current window a *different* window, the situation that
# lost the pane at 07:52, and confirm the option still belongs to the pane.
other=$($TMUX new-window -d -P -F '#{window_id}' -t "=$SESSION:")
$TMUX select-window -t "$other"
serve_pane web "$PORT" "$APP_DIR/good.sh $PORT" / >"$APP_DIR/boot2.log" 2>&1
rc=$?
check "serve_pane: restarts cleanly while another window is current" "$rc" "0"
webpane=$(pane_for web)
check "remain-on-exit: still on the server pane, not the current window" \
  "$($TMUX show-options -p -t "$webpane" remain-on-exit | awk '{print $2}')" "on"

### FINDING 2: a crash keeps the pane and its output -----------------------
cat >"$APP_DIR/crash.sh" <<'EOF'
#!/usr/bin/env bash
echo "BOOT FAILED: something specific and greppable"
exit 1
EOF
chmod +x "$APP_DIR/crash.sh"

start=$(date +%s)
serve_pane web "$PORT" "$APP_DIR/crash.sh" / >"$APP_DIR/boot3.log" 2>&1
rc=$?
took=$(($(date +%s) - start))
check "crash: serve_pane fails" "$rc" "1"
check "crash: fails fast, no 60s spin (took ${took}s)" "$(( took < 20 ))" "1"
if grep -q "exited during boot" "$APP_DIR/boot3.log"; then
  ok "crash: reported as exited during boot"
else
  bad "crash: reported as exited during boot" "$(tail -3 "$APP_DIR/boot3.log")"
fi
if grep -q "BOOT FAILED: something specific and greppable" "$APP_DIR/boot3.log"; then
  ok "crash: the server's own output was captured"
else
  bad "crash: the server's own output was captured" "$(tail -5 "$APP_DIR/boot3.log")"
fi

### FINDING 2: a clean exit closes the pane; must fail fast, not spin ------
cat >"$APP_DIR/quit.sh" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$APP_DIR/quit.sh"

start=$(date +%s)
serve_pane web "$PORT" "$APP_DIR/quit.sh" / >"$APP_DIR/boot4.log" 2>&1
rc=$?
took=$(($(date +%s) - start))
check "clean exit: serve_pane fails" "$rc" "1"
check "clean exit: fails fast, no 60s spin (took ${took}s)" "$(( took < 20 ))" "1"
if grep -q "pane is gone" "$APP_DIR/boot4.log"; then
  ok "clean exit: reported as a vanished pane"
else
  bad "clean exit: reported as a vanished pane" "$(tail -3 "$APP_DIR/boot4.log")"
fi

### The session survived all of it -----------------------------------------
if $TMUX has-session -t "=$SESSION" 2>/dev/null; then
  ok "session '$SESSION' still exists after every restart and crash"
else
  bad "session '$SESSION' still exists" "the session was destroyed"
fi

echo
echo "== $pass passed, $fail failed"
[ "$fail" -eq 0 ]

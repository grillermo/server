# shellcheck shell=bash
# Sourced by a personal service's ./serve to (re)start its server in a pane
# of a detached tmux session, in place, then return. Usage:
#
#   SESSION=rulinky APP_DIR=$(cd "$(dirname "$0")" && pwd)
#   . ~/c/server/serve-in-place.sh
#   serve_pane web 3002 "env RAILS_ENV=production bin/rails server -p 3002" /
#   serve_attach
#
# Why in place: git-sync runs ./activate -> ./serve after landing new code,
# with no tty. A foreground server would start a second copy beside the one
# in tmux. Pane commands go through top-cpu-service-wrapper, as
# after-reboot.sh's do, so monitord still attributes the server to its service.

: "${SESSION:?set SESSION before sourcing serve-in-place.sh}"
: "${APP_DIR:?set APP_DIR before sourcing serve-in-place.sh}"

# No login shell when git-sync runs us; tmux panes inherit this PATH below.
export PATH="$HOME/.rbenv/shims:$HOME/Library/pnpm:$HOME/.bun/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"

SERVE_WRAPPER="$HOME/c/server/top-cpu-service-wrapper"

port_listeners() {
  lsof -ti "tcp:$1" -sTCP:LISTEN 2>/dev/null || true
}

# The previous occupant of our port is always ours (including a foreground
# run of an old ./serve), so reclaim it before respawning the pane.
free_port() {
  local port="$1" pids
  pids=$(port_listeners "$port")
  [ -n "$pids" ] || return 0
  echo "==> Port $port held by PID(s): $(echo "$pids" | tr '\n' ' ')- stopping"
  # shellcheck disable=SC2086
  kill $pids 2>/dev/null || true
  for _ in $(seq 1 20); do
    sleep 0.5
    [ -n "$(port_listeners "$port")" ] || return 0
  done
  pids=$(port_listeners "$port")
  # shellcheck disable=SC2086
  [ -z "$pids" ] || kill -9 $pids 2>/dev/null || true
  sleep 1
  [ -z "$(port_listeners "$port")" ] || { echo "ERROR: could not free port $port."; return 1; }
}

pane_for() {
  tmux has-session -t "=$SESSION" 2>/dev/null || return 0
  tmux list-panes -t "=$SESSION:" -F '#{pane_title} #{pane_id}' 2>/dev/null |
    awk -v t="$1" '$1==t{print $2}'
}

pane_dead() {
  [ "$(tmux display-message -p -t "$1" '#{pane_dead}' 2>/dev/null)" = "1" ]
}

# start_pane <title> <argv...>: respawn the titled pane running argv through
# the wrapper; remain-on-exit keeps a pane that crashed on boot, with output.
start_pane() {
  local title="$1" cmd="$2" pid
  pid=$(pane_for "$title")
  if [ -z "$pid" ]; then
    if tmux has-session -t "=$SESSION" 2>/dev/null; then
      pid=$(tmux split-window -P -F '#{pane_id}' -t "=$SESSION:")
    else
      tmux new-session -d -s "$SESSION" -n main -x 200 -y 50
      pid=$(tmux list-panes -t "=$SESSION:" -F '#{pane_id}' | head -1)
    fi
    tmux select-pane -t "$pid" -T "$title"
  fi
  tmux set-option -t "=$SESSION:" remain-on-exit on >/dev/null
  tmux respawn-pane -k -t "$pid" \
    "cd '$APP_DIR' && export PATH='$PATH' && exec '$SERVE_WRAPPER' '$SESSION' '$APP_DIR' $cmd"
  tmux select-pane -t "$pid" -T "$title" # respawn-pane resets it on some tmux versions
  tmux select-layout -t "=$SESSION:" even-horizontal >/dev/null
}

dump_pane() {
  local pid
  pid=$(pane_for "$1")
  [ -n "$pid" ] || { echo "(no $1 pane to inspect)"; return 0; }
  echo "----- last lines of $1 pane -----"
  tmux capture-pane -p -S -500 -t "$pid" 2>/dev/null | grep -v '^$' | tail -n 25 || true
  echo "---------------------------------"
}

# wait_up <title> <port> <path>: up once the path answers below 500.
wait_up() {
  local title="$1" port="$2" path="$3" pid code
  pid=$(pane_for "$title")
  echo "==> Waiting for $title on port $port..."
  for _ in $(seq 1 60); do
    code=$(curl -s -o /dev/null -m 2 -w '%{http_code}' "http://127.0.0.1:$port$path" || true)
    if [ "$code" != 000 ] && [ "$code" -lt 500 ]; then
      echo "==> $title up (HTTP $code on $path)"
      return 0
    fi
    if [ -z "$pid" ] || pane_dead "$pid"; then
      echo "ERROR: $title exited during boot."
      dump_pane "$title"
      return 1
    fi
    sleep 1
  done
  echo "ERROR: $title not up after 60s."
  dump_pane "$title"
  return 1
}

# serve_pane <title> <port> <command> [<health path>]
serve_pane() {
  local title="$1" port="$2" cmd="$3" path="${4:-/}"
  echo "==> (Re)starting $title (port $port)"
  free_port "$port" || return 1
  start_pane "$title" "$cmd"
  wait_up "$title" "$port" "$path"
}

# serve_aux <title> <command>: a pane with no port to wait on (a watcher).
serve_aux() {
  echo "==> (Re)starting $1"
  start_pane "$1" "$2"
}

# Attach only when a human is watching; git-sync has no tty.
serve_attach() {
  if [ -n "${TMUX:-}" ] && [ -t 1 ]; then
    tmux switch-client -t "=$SESSION"
  elif [ -t 1 ]; then
    tmux attach-session -t "=$SESSION"
  else
    echo "==> No TTY; not attaching. Run: tmux attach -t $SESSION"
  fi
}

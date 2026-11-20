# shellcheck shell=bash
# Sourced by a personal service's ./serve to (re)start its server in a pane
# of a detached tmux session, in place, then return. Usage:
#
#   SESSION=rulinky APP_DIR=$(cd "$(dirname "$0")" && pwd)
#   . ~/c/server/serve-in-place.sh
#   serve_require_clean_main          # optional: refuse half-done checkouts
#   serve_load_env                    # optional: .env for the script and panes
#   serve_pane web 3002 "env RAILS_ENV=production bin/rails server -p 3002" /
#   serve_attach
#
# Why in place: git-sync runs ./activate -> ./serve after landing new code,
# with no tty. A foreground server would start a second copy beside the one
# in tmux. The first run creates the session; every later run restarts each
# titled pane inside it and never creates, kills or switches to a session.

: "${SESSION:?set SESSION before sourcing serve-in-place.sh}"
: "${APP_DIR:?set APP_DIR before sourcing serve-in-place.sh}"

# No login shell when git-sync runs us; tmux panes inherit this PATH below.
export PATH="$HOME/.rbenv/shims:$HOME/Library/pnpm:$HOME/.bun/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"

# Set by serve_load_env; each pane command sources it again, because a pane
# gets the tmux server's environment, not this script's.
SERVE_ENV_FILE=""

# serve_require_clean_main: deploy only committed work. Refuses when the
# checkout is not on main or tracked files have uncommitted changes.
# Untracked files (logs, .env, scratch) don't count. A service living in a
# subfolder of a busy repo sets SERVE_SCOPE=. (a path relative to APP_DIR) so
# only changes under it block the deploy; unset, the whole repo is checked.
serve_require_clean_main() {
  local branch scope=()
  branch=$(git -C "$APP_DIR" symbolic-ref --short -q HEAD || echo "(detached)")
  if [ "$branch" != main ]; then
    echo "ERROR: $APP_DIR is on '$branch', not main. Not deploying."
    exit 1
  fi
  [ -z "${SERVE_SCOPE:-}" ] || scope=(-- "$SERVE_SCOPE")
  if [ -n "$(git -C "$APP_DIR" status --porcelain --untracked-files=no ${scope[@]+"${scope[@]}"})" ]; then
    echo "ERROR: uncommitted changes in $APP_DIR${SERVE_SCOPE:+ (scope $SERVE_SCOPE)}. Commit or stash them; not deploying:"
    git -C "$APP_DIR" status --short --untracked-files=no ${scope[@]+"${scope[@]}"}
    exit 1
  fi
}

# serve_load_env [file]: export the dotenv file (default .env) into this
# script, and have every pane started after this source it too.
serve_load_env() {
  local file="$APP_DIR/${1:-.env}"
  [ -f "$file" ] || return 0
  set -a
  # shellcheck source=/dev/null
  . "$file"
  set +a
  SERVE_ENV_FILE="$file"
}

# Never Caddy: on the macmini it also listens on 192.168.1.1:<app port> to
# make loopback-bound apps reachable from the LAN (see the Caddyfile), and
# killing it here would take every site down on each deploy.
port_listeners() {
  lsof -ti "tcp:$1" -sTCP:LISTEN -a -c '^caddy' 2>/dev/null || true
}

# The previous occupant of our port is always ours (including a foreground
# run of an old ./serve), so reclaim it before respawning the pane. Wait for
# the processes themselves to exit, not just the port: Puma closes its
# listener first and keeps running while it drains (Solid Queue in Puma can
# take a while), and a server started meanwhile finds the old pidfile alive
# and quits with "A server is already running".
pids_alive() {
  local p
  for p in $1; do kill -0 "$p" 2>/dev/null && return 0; done
  return 1
}

# pidfile_pids: the PID in the app's server pidfile, when it is still alive.
# The port alone is not enough to find the old server: by the time we look,
# a Puma killed by park_pane has often already closed its listener while the
# process lives on (draining, Solid Queue in Puma), and a server started then
# finds that pidfile alive and quits with "A server is already running". An
# app whose pidfile is elsewhere sets SERVE_PIDFILE (relative to APP_DIR).
pidfile_pids() {
  local f="$APP_DIR/${SERVE_PIDFILE:-tmp/pids/server.pid}" pid
  [ -f "$f" ] || return 0
  pid=$(tr -dc '0-9' <"$f")
  [ -n "$pid" ] || return 0
  kill -0 "$pid" 2>/dev/null && echo "$pid"
  return 0
}

# port_owners <port>: everything that must die before the new server starts -
# whoever listens on the port, plus the pidfile's owner.
port_owners() {
  {
    port_listeners "$1"
    pidfile_pids
  } | awk 'NF' | sort -u
}

free_port() {
  local port="$1" pids
  pids=$(port_owners "$port")
  [ -n "$pids" ] || return 0
  echo "==> Port $port held by PID(s): $(echo "$pids" | tr '\n' ' ')- stopping"
  # shellcheck disable=SC2086
  kill $pids 2>/dev/null || true
  for _ in $(seq 1 60); do
    sleep 0.5
    if ! pids_alive "$pids" && [ -z "$(port_owners "$port")" ]; then
      return 0
    fi
  done
  echo "==> PID(s) still running after 30s - killing"
  # shellcheck disable=SC2086,SC2046
  kill -9 $pids $(port_owners "$port") 2>/dev/null || true
  sleep 1
  [ -z "$(port_owners "$port")" ] || { echo "ERROR: could not free port $port."; return 1; }
}

# pane_for <title>: the pane start_pane gave that title. It is kept in the
# pane option @serve-title as well, because the visible title can drift
# (tmux-resurrect restores old ones), and respawning by a drifted title kills
# another server. A pane without the option is matched by its title.
pane_for() {
  tmux has-session -t "=$SESSION" 2>/dev/null || return 0
  tmux list-panes -s -t "=$SESSION" -F '#{@serve-title}|#{pane_title}|#{pane_id}' 2>/dev/null |
    awk -F'|' -v t="$1" '$1==t{tagged=$3} $1=="" && $2==t && !untagged{untagged=$3}
      END{if (tagged) print tagged; else if (untagged) print untagged}'
}

pane_dead() {
  [ "$(tmux display-message -p -t "$1" '#{pane_dead}' 2>/dev/null)" = "1" ]
}

# pane_gone <pane id>: the pane does not exist any more. Must be asked
# separately from pane_dead, which answers "" (not "1") for a pane that is
# gone rather than dead, so a loop testing only pane_dead never notices.
pane_gone() {
  [ -z "$(tmux display-message -p -t "$1" '#{pane_id}' 2>/dev/null)" ]
}

# caller_window: the window of the pane running this script, when that pane
# is inside $SESSION (someone ran ./serve from a shell in the session, e.g.
# the after-reboot launcher or an interactive session that shares the name).
caller_window() {
  [ -n "${TMUX_PANE:-}" ] || return 0
  [ "$(tmux display-message -p -t "$TMUX_PANE" '#{session_name}' 2>/dev/null)" = "$SESSION" ] || return 0
  tmux display-message -p -t "$TMUX_PANE" '#{window_id}'
}

# serve_window <caller window>: a window already holding server panes, other
# than the caller's.
serve_window() {
  tmux list-panes -s -t "=$SESSION" -F '#{@serve-title}|#{window_id}' 2>/dev/null |
    awk -F'|' -v c="$1" '$1!="" && $2!=c {print $2; exit}'
}

# start_pane <title> <command>: respawn the titled pane running the command;
# remain-on-exit keeps a pane that crashed on boot, with output. A session's
# @serve-layout option, when set, overrides the even-horizontal layout.
# Never takes over the caller's view: a server pane never lands in (or stays
# in) the caller's window, and the pane that was active stays active.
start_pane() {
  local title="$1" cmd="$2" pid layout env_load="" caller win prev=""
  caller=$(caller_window)
  if tmux has-session -t "=$SESSION" 2>/dev/null; then
    prev=$(tmux display-message -p -t "=$SESSION:" '#{pane_id}')
  fi
  pid=$(pane_for "$title")
  if [ -n "$pid" ] && [ -n "$caller" ] &&
    [ "$(tmux display-message -p -t "$pid" '#{window_id}')" = "$caller" ]; then
    win=$(serve_window "$caller")
    if [ -n "$win" ]; then
      tmux join-pane -d -s "$pid" -t "$win"
    else
      tmux break-pane -d -s "$pid" -n serve
    fi
  fi
  if [ -z "$pid" ]; then
    if tmux has-session -t "=$SESSION" 2>/dev/null; then
      win=$(serve_window "$caller")
      if [ -n "$win" ]; then
        pid=$(tmux split-window -d -P -F '#{pane_id}' -t "$win")
      elif [ -n "$caller" ]; then
        pid=$(tmux new-window -d -n serve -P -F '#{pane_id}' -t "=$SESSION:")
      else
        pid=$(tmux split-window -d -P -F '#{pane_id}' -t "=$SESSION:")
      fi
    else
      tmux new-session -d -s "$SESSION" -n main -x 200 -y 50
      pid=$(tmux list-panes -t "=$SESSION:" -F '#{pane_id}' | head -1)
    fi
  fi
  [ -z "$SERVE_ENV_FILE" ] || env_load="set -a && . '$SERVE_ENV_FILE' && set +a && "
  # Keep this pane if its server dies on boot, so its output survives. Set on
  # the pane itself: "=$SESSION:" targets the session's *current* window, which
  # since server panes moved out of the caller's window is usually not the
  # window holding them - a crashing pane then closed and took its output with
  # it ("no web pane to inspect"). tmux.conf's pane-died hook still closes a
  # pane whose process was stopped on purpose.
  tmux set-option -p -t "$pid" remain-on-exit on >/dev/null
  tmux respawn-pane -k -t "$pid" "cd '$APP_DIR' && export PATH='$PATH' && ${env_load}exec $cmd"
  tmux select-pane -t "$pid" -T "$title" # respawn-pane resets it on some tmux versions
  tmux set-option -p -t "$pid" @serve-title "$title"
  layout=$(tmux show-options -qv -t "=$SESSION:" @serve-layout 2>/dev/null)
  tmux select-layout -t "$pid" "${layout:-even-horizontal}" >/dev/null
  # select-pane -T also focuses the pane; hand focus back.
  if [ -n "$prev" ] && [ "$prev" != "$pid" ]; then
    tmux select-window -t "$prev" 2>/dev/null || true
    tmux select-pane -t "$prev" 2>/dev/null || true
  fi
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
    if pane_gone "$pid"; then
      echo "ERROR: $title's pane is gone; its output went with it."
      echo "       (the process exited cleanly or on a signal, so the pane-died hook closed it)"
      return 1
    fi
    sleep 1
  done
  echo "ERROR: $title not up after 60s."
  dump_pane "$title"
  return 1
}

# park_pane <title>: swap the pane's server for a placeholder before the port
# is freed. Killing the server in place would close the pane (tmux.conf's
# pane-died hook closes panes stopped on purpose), and with it a session that
# has no other pane, so the restart would create a new session. respawn-pane
# keeps the pane; free_port then stops anything still on the port (Puma
# ignores the SIGHUP respawn-pane sends).
park_pane() {
  local pid
  pid=$(pane_for "$1")
  [ -n "$pid" ] || return 0
  tmux respawn-pane -k -t "$pid" "sleep 86400"
}

# serve_pane <title> <port> <command> [<health path>]
serve_pane() {
  local title="$1" port="$2" cmd="$3" path="${4:-/}"
  echo "==> (Re)starting $title (port $port)"
  park_pane "$title"
  free_port "$port" || return 1
  start_pane "$title" "$cmd"
  wait_up "$title" "$port" "$path"
}

# serve_aux <title> <command>: a pane with no port to wait on (a watcher).
serve_aux() {
  echo "==> (Re)starting $1"
  start_pane "$1" "$2"
}

# serve_stop <title> [<seconds>]: stop a pane's process gracefully (SIGTERM,
# so Puma finishes requests and Solid Queue its jobs) and let the pane close;
# kill the pane if it is still there after the timeout (default 60s). Used to
# retire the old slot of a blue/green deploy.
serve_stop() {
  local title="$1" timeout="${2:-60}" pid
  pid=$(pane_for "$title")
  [ -n "$pid" ] || return 0
  echo "==> Stopping $title"
  kill -TERM "$(tmux display-message -p -t "$pid" '#{pane_pid}')" 2>/dev/null || true
  for _ in $(seq 1 "$timeout"); do
    [ -n "$(pane_for "$title")" ] || return 0
    pane_dead "$pid" && break
    sleep 1
  done
  tmux kill-pane -t "$pid" 2>/dev/null || true
}

# serve_record_deploy: append "<time> <commit>" to log/deploys.log, so a bad
# deploy can be traced to the commits to revert on main.
serve_record_deploy() {
  mkdir -p "$APP_DIR/log"
  echo "$(date '+%Y-%m-%dT%H:%M:%S%z') $(git -C "$APP_DIR" rev-parse --short HEAD)" >>"$APP_DIR/log/deploys.log"
}

# serve_lan_url <port>: http://<this machine's LAN IP>:<port>, or nothing.
serve_lan_url() {
  local ip
  ip=$(ipconfig getifaddr en0 2>/dev/null || hostname -I 2>/dev/null | awk '{print $1}' || true)
  [ -z "$ip" ] || echo "http://$ip:$1"
}

# serve_check_public <url>: warn, never fail, when the public URL is down.
serve_check_public() {
  local code
  code=$(curl -s -o /dev/null -m 10 -w '%{http_code}' "$1" || true)
  if [ "$code" != 000 ] && [ "$code" -lt 500 ]; then
    echo "==> $1 is up (HTTP $code)"
  else
    echo "WARNING: $1 answered $code. The app is up locally; check the tunnel or DNS."
  fi
}

# Never attach or switch-client: ./serve runs from Claude's tmux pane,
# git-sync and by hand, and in all of them the caller's terminal must stay
# where it is. Watch the session from another terminal instead.
serve_attach() {
  echo "==> Session '$SESSION' updated. Watch it with: tmux attach -t $SESSION"
}

#!/usr/bin/env bash
# description: Description comes here.... tmux script runs on startup.
#
rm /opt/homebrew/var/postgresql@18/postmaster.pid
brew services start redis&

# Base directory for all projects
BASE_DIR="/Users/grillermo/c"

# Array of all your services. Each entry is a directory under $BASE_DIR, run
# with ./serve; "dir:script" runs a different script from that directory.
SERVICES=(
    "kimai"
    "rulinky"
    "auto-email-classifier"
    "labs"
    "serve-html-markdown"
    "ebooks"
    "readitsoon"
    "blog_grillermo_com"
    "file_server"
    "yosubee"
    "awh"
    "patatatube"
    "comunidad-antesis"
    "comunidad-antesis-staging:./serve-staging"
    "torlink"
    "ntfyllermo"
    "my-claude-usage"
    "virtual-artifact-renderer"
)

# Function to create and configure a tmux session
setup_tmux_session() {
    local dir_name=${1%%:*}
    # A nested dir (a/b) names its session after the last part.
    local session_name=${dir_name##*/}
    local serve_script=./serve
    [[ $1 == *:* ]] && serve_script=${1#*:}
    
    local full_path="$BASE_DIR/$dir_name"

    echo "Creating session: $session_name in $full_path"
    
    # Create the session in the background
    tmux new-session -d -s "$session_name"
    
    # --- PANE 1: The Log Tail (Left Side) ---
    tmux send-keys -t "$session_name" "cd $full_path" C-m
    # ./serve sources serve-in-place.sh, which starts the server in its own
    # titled pane through top-cpu-service-wrapper and returns, leaving this
    # pane an ordinary shell. Services whose ./serve doesn't source
    # serve-in-place.sh yet run in the foreground here, unwrapped, until
    # converted.
    tmux send-keys -t "$session_name" "$serve_script" C-m
    
    # --- PANE 2: The Code Editor (Right Side) ---
    tmux split-window -h -t "$session_name"
    tmux send-keys -t "$session_name" "cd $full_path" C-m
}

# Start the tmux-server 
tmux start-server


# Loop through the array and apply the function
for service in "${SERVICES[@]}"; do
    setup_tmux_session "$service"
done

echo "All tmux sessions created!"

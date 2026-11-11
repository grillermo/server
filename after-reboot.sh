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
    "rulinky"
    "auto-email-classifier"
    "labs"
    "serve-html-markdown"
    "ebooks"
    "readitsoon"
    "narratey:app/serve"
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
    "qotnews"
    "sota-llms"
    "chat-chiq-me"
    "third-parties/twenty"
    "daisolar/cotizacion-generator"
    "daisolar/bot:./fake-daisolar-bot/serve"
    "compras"
    "ventas-libres:./server/serve"
    "cal.diy"
    "my-articles"
    "plannotator-router"
    "bank-app-parser"
    "third-parties/claudecodeui"
    "pocket-id"
)

# Function to create and configure a tmux session
setup_tmux_session() {
    local dir_name=${1%%:*}
    # A nested dir (a/b) names its session after the last part.
    local session_name=${dir_name##*/}
    # tmux renames "a.b" to "a_b" but reads "-t a.b" as a pane target.
    session_name=${session_name//./_}
    local serve_script=./serve
    [[ $1 == *:* ]] && serve_script=${1#*:}
    
    local full_path="$BASE_DIR/$dir_name"

    echo "Creating session: $session_name in $full_path"
    
    # Create the session in the background. ZSH_UNATTENDED keeps zsh from
    # asking anything (update, dotenv) that would eat the keys sent below;
    # it is dropped from the session afterwards so panes you open later ask.
    tmux new-session -d -s "$session_name" -e ZSH_UNATTENDED=1
    
    # --- PANE 1: The Log Tail (Left Side) ---
    tmux send-keys -t "$session_name" "cd $full_path" C-m
    # ./serve sources serve-in-place.sh, which starts the server in its own
    # titled pane of this session and returns; this launcher pane then exits
    # to save RAM, or stays open showing the error if ./serve failed. Services
    # whose ./serve doesn't source serve-in-place.sh yet run in the foreground
    # here until converted (the serve-script skill).
    tmux send-keys -t "$session_name" "$serve_script && exit" C-m

    tmux set-environment -u -t "$session_name" ZSH_UNATTENDED
}

# Start the tmux-server 
tmux start-server


# Loop through the array and apply the function
for service in "${SERVICES[@]}"; do
    setup_tmux_session "$service"
done

echo "All tmux sessions created!"

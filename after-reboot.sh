#!/usr/bin/env bash
# description: Description comes here.... tmux script runs on startup.
#
rm /opt/homebrew/var/postgresql@18/postmaster.pid

# Base directory for all projects
BASE_DIR="/Users/grillermo/c"
TOP_CPU_WRAPPER="/Users/grillermo/c/server/top-cpu-service-wrapper"

# Array of all your services
SERVICES=(
    "kimai"
    "rulinky"
    "auto-email-classifier"
    "labs"
    "serve-html-markdown"
    "ebooks"
    "readitsoon"
    "blog_grillermo_com"
    "file_to_s3"
    "yosubee"
    "awh"
    "patatatube"
    "top_cpu"
)

# Function to create and configure a tmux session
setup_tmux_session() {
    local session_name=$1
    local dir_name=$session_name
    
    local full_path="$BASE_DIR/$dir_name"

    echo "Creating session: $session_name in $full_path"
    
    # Create the session in the background
    tmux new-session -d -s "$session_name"
    
    # --- PANE 1: The Log Tail (Left Side) ---
    tmux send-keys -t "$session_name" "cd $full_path" C-m
    # Touch the log file just in case Monit hasn't created it yet
    tmux send-keys -t "$session_name" "exec -a \"$session_name\" \"$TOP_CPU_WRAPPER\" \"$session_name\" \"$full_path\" ./serve" C-m
    
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

# Monit will monitor the services now because it is installed through

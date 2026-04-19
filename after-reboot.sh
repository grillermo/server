#!/bin/bash
# description: Description comes here.... tmux script runs on startup.

rm /opt/homebrew/var/postgresql@18/postmaster.pid

tmux new-session -d -s rulinky
tmux new-session -d -s kimai
tmux new-session -d -s auto-email-classifier
tmux new-session -d -s labs
tmux new-session -d -s serve-html-markdown
tmux new-session -d -s ebooks
tmux new-session -d -s read-it-soon
tmux new-session -d -s grillermo_com
tmux new-session -d -s file_to_s3

tmux send-keys -t kimai "cd /Users/grillermo/c/kimai" C-m
tmux send-keys -t rulinky "cd /Users/grillermo/c/rulinky" C-m
tmux send-keys -t auto-email-classifier "cd /Users/grillermo/c/auto-email-classifier/rules_editor" C-m
tmux send-keys -t labs "cd /Users/grillermo/c/labs" C-m
tmux send-keys -t serve-html-markdown "cd /Users/grillermo/c/serve-html-markdown" C-m
tmux send-keys -t ebooks "cd /Users/grillermo/c/calibreshare" C-m
tmux send-keys -t read-it-soon "cd /Users/grillermo/c/read-it-soon" C-m
tmux send-keys -t grillermo_com "cd /Users/grillermo/c/grillermo_com" C-m
tmux send-keys -t file_to_s3 "cd /Users/grillermo/c/file_to_s3" C-m

tmux send-keys -t kimai "./serve" C-m
tmux send-keys -t rulinky "./serve" C-m
tmux send-keys -t auto-email-classifier "./serve" C-m
tmux send-keys -t labs "./serve" C-m
tmux send-keys -t serve-html-markdown "source python-env/bin/activate && ./serve" C-m
tmux send-keys -t ebooks "./serve" C-m
tmux send-keys -t read-it-soon "./serve" C-m
tmux send-keys -t grillermo_com "./serve" C-m
tmux send-keys -t file_to_s3 "./serve" C-m

#!/usr/bin/env bash

# Base directory for all projects
BASE_DIR="/Users/grillermo/c"

# Array of all your services
SERVICES=(
    "kimai"
    "rulinky"
    "auto-email-classifier"
    "labs"
    "serve-html-markdown"
    "ebooks"
    "read-it-soon"
    "grillermo_com"
    "file_to_s3"
)

# Function to create and configure a tmux session
setup_tmux_session() {
    local session_name=$1
    local dir_name=$session_name
    
    # Handle the exceptions where the folder name doesn't match the session name
    case $session_name in
        "auto-email-classifier") dir_name="auto-email-classifier/rules_editor" ;;
        "ebooks") dir_name="calibreshare" ;;
    esac
    
    local full_path="$BASE_DIR/$dir_name"

    echo "Creating session: $session_name in $full_path"
    
    # Create the session in the background
    tmux new-session -d -s "$session_name"
    
    # --- PANE 1: The Log Tail (Left Side) ---
    tmux send-keys -t "$session_name" "cd $full_path" C-m
    # Touch the log file just in case Monit hasn't created it yet
    tmux send-keys -t "$session_name" "touch output.log && tail -f output.log" C-m
    
    # --- PANE 2: The Code Editor (Right Side) ---
    tmux split-window -h -t "$session_name"
    tmux send-keys -t "$session_name" "cd $full_path" C-m
    # If it's the markdown service, activate the python environment in the editor pane too
    if [ "$session_name" == "serve-html-markdown" ]; then
        tmux send-keys -t "$session_name" "source python-env/bin/activate" C-m
    fi
}

# Loop through the array and apply the function
for service in "${SERVICES[@]}"; do
    setup_tmux_session "$service"
done

echo "All tmux sessions created!"


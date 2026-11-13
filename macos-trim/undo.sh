#!/bin/bash
# Re-enable every agent disabled by the server trim; a logout/reboot restarts them.
cd "$(dirname "$0")" || exit 1
while read -r l; do launchctl enable "gui/$(id -u)/$l"; done < labels.txt
echo "Re-enabled. Log out and back in (or reboot) to start them again."

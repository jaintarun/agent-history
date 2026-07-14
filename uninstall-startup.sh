#!/bin/sh
set -eu

label="com.tarunjain.agent-history"
domain="gui/$(id -u)"
service="$domain/$label"
repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
plist_path="$HOME/Library/LaunchAgents/$label.plist"
removed=false

if [ "$(uname -s)" != "Darwin" ]; then
  printf '%s\n' "This uninstaller supports macOS only." >&2
  exit 1
fi

if launchctl print "$service" >/dev/null 2>&1; then
  launchctl bootout "$service"
  attempts=0
  while launchctl print "$service" >/dev/null 2>&1; do
    attempts=$((attempts + 1))
    if [ "$attempts" -ge 50 ]; then
      printf 'Timed out waiting for the Agent History job to stop.\n' >&2
      exit 1
    fi
    sleep 0.1
  done
  removed=true
fi
launchctl disable "$service" >/dev/null 2>&1 || true

if [ -f "$plist_path" ]; then
  rm -f "$plist_path"
  removed=true
fi

listener_pids=$(lsof -tiTCP:54321 -sTCP:LISTEN 2>/dev/null || true)
for pid in $listener_pids; do
  command=$(ps -p "$pid" -o command= 2>/dev/null || true)
  process_cwd=$(lsof -a -p "$pid" -d cwd -Fn 2>/dev/null | sed -n 's/^n//p' || true)
  case "$command" in
    *"agent-history serve"*)
      if [ "$process_cwd" = "$repo_dir" ]; then
        kill -TERM "$pid"
        attempts=0
        while kill -0 "$pid" 2>/dev/null && [ "$attempts" -lt 50 ]; do
          sleep 0.1
          attempts=$((attempts + 1))
        done
        if kill -0 "$pid" 2>/dev/null; then
          printf 'Timed out waiting for Agent History process %s to stop.\n' "$pid" >&2
          exit 1
        fi
        removed=true
      fi
      ;;
  esac
done

if [ "$removed" = true ]; then
  printf 'Agent History startup removed. Data and the application were kept.\n'
else
  printf 'Agent History startup is already removed.\n'
fi

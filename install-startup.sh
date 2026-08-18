#!/bin/sh
set -eu

label="io.github.jaintarun.agent-history"
legacy_label="com.tarunjain.agent-history"
domain="gui/$(id -u)"
service="$domain/$label"
legacy_service="$domain/$legacy_label"
repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
run_script="$repo_dir/run-local.sh"
plist_dir="$HOME/Library/LaunchAgents"
plist_path="$plist_dir/$label.plist"
legacy_plist_path="$plist_dir/$legacy_label.plist"
log_dir="$HOME/Library/Logs"
runtime_path="/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$HOME/.local/bin:/Applications/cmux.app/Contents/Resources/bin"

if [ "$(uname -s)" != "Darwin" ]; then
  printf '%s\n' "This installer supports macOS only." >&2
  exit 1
fi
if [ ! -f "$run_script" ]; then
  printf 'Missing startup command: %s\n' "$run_script" >&2
  exit 1
fi

mkdir -p "$plist_dir" "$log_dir"
tmp_plist=$(mktemp "${TMPDIR:-/tmp}/$label.XXXXXX")
rm -f "$tmp_plist"
bootstrap_error=""
cleanup() {
  [ -z "$tmp_plist" ] || rm -f "$tmp_plist"
  [ -z "$bootstrap_error" ] || rm -f "$bootstrap_error"
}
trap cleanup EXIT HUP INT TERM

plistbuddy=/usr/libexec/PlistBuddy
"$plistbuddy" -c "Add :Label string $label" "$tmp_plist" >/dev/null
"$plistbuddy" -c "Add :ProgramArguments array" "$tmp_plist" >/dev/null
"$plistbuddy" -c "Add :ProgramArguments:0 string /bin/sh" "$tmp_plist" >/dev/null
"$plistbuddy" -c "Add :ProgramArguments:1 string $run_script" "$tmp_plist" >/dev/null
"$plistbuddy" -c "Add :WorkingDirectory string $repo_dir" "$tmp_plist" >/dev/null
"$plistbuddy" -c "Add :EnvironmentVariables dict" "$tmp_plist" >/dev/null
"$plistbuddy" -c "Add :EnvironmentVariables:PATH string $runtime_path" "$tmp_plist" >/dev/null
"$plistbuddy" -c "Add :EnvironmentVariables:AGENT_HISTORY_LOG_PATH string $log_dir/agent-history.error.log" "$tmp_plist" >/dev/null
"$plistbuddy" -c "Add :RunAtLoad bool true" "$tmp_plist" >/dev/null
"$plistbuddy" -c "Add :KeepAlive bool true" "$tmp_plist" >/dev/null
"$plistbuddy" -c "Add :ProcessType string Background" "$tmp_plist" >/dev/null
"$plistbuddy" -c "Add :ThrottleInterval integer 15" "$tmp_plist" >/dev/null
"$plistbuddy" -c "Add :StandardOutPath string $log_dir/agent-history.log" "$tmp_plist" >/dev/null
"$plistbuddy" -c "Add :StandardErrorPath string $log_dir/agent-history.startup.log" "$tmp_plist" >/dev/null
plutil -lint "$tmp_plist" >/dev/null

if launchctl print "$legacy_service" >/dev/null 2>&1; then
  launchctl bootout "$legacy_service"
  attempts=0
  while launchctl print "$legacy_service" >/dev/null 2>&1; do
    attempts=$((attempts + 1))
    if [ "$attempts" -ge 50 ]; then
      printf 'Timed out waiting for the legacy Agent History job to stop.\n' >&2
      exit 1
    fi
    sleep 0.1
  done
fi
launchctl disable "$legacy_service" >/dev/null 2>&1 || true
rm -f "$legacy_plist_path"

loaded=false
if launchctl print "$service" >/dev/null 2>&1; then
  loaded=true
fi
if [ "$loaded" = true ]; then
  launchctl bootout "$service"
  attempts=0
  while launchctl print "$service" >/dev/null 2>&1; do
    attempts=$((attempts + 1))
    if [ "$attempts" -ge 50 ]; then
      printf 'Timed out waiting for the previous Agent History job to stop.\n' >&2
      exit 1
    fi
    sleep 0.1
  done
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
      else
        printf 'Port 54321 is used by Agent History from another folder: %s\n' "$process_cwd" >&2
        exit 1
      fi
      ;;
    *)
      printf 'Port 54321 is already used by another process: %s\n' "$command" >&2
      exit 1
      ;;
  esac
done

mv "$tmp_plist" "$plist_path"
tmp_plist=""
chmod 600 "$plist_path"
launchctl enable "$service"
bootstrap_error=$(mktemp "${TMPDIR:-/tmp}/$label.bootstrap.XXXXXX")
attempts=0
until launchctl bootstrap "$domain" "$plist_path" >"$bootstrap_error" 2>&1; do
  attempts=$((attempts + 1))
  if [ "$attempts" -ge 50 ]; then
    cat "$bootstrap_error" >&2
    rm -f "$plist_path"
    exit 1
  fi
  sleep 0.1
done
rm -f "$bootstrap_error"
bootstrap_error=""

attempts=0
until curl -fsS http://127.0.0.1:54321/api/health >/dev/null 2>&1; do
  attempts=$((attempts + 1))
  if [ "$attempts" -ge 100 ]; then
    printf 'Agent History was installed but did not become healthy. Check %s/agent-history.startup.log\n' "$log_dir" >&2
    exit 1
  fi
  sleep 0.1
done

printf 'Agent History startup installed and running.\n%s\n' "http://127.0.0.1:54321/"

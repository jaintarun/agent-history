package launch

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStartupScriptsAreIdempotent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS LaunchAgent test")
	}

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	sourceRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	repoRoot := filepath.Join(t.TempDir(), "Agent History Repo")
	if err := os.MkdirAll(repoRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"install-startup.sh", "uninstall-startup.sh", "run-local.sh"} {
		content, err := os.ReadFile(filepath.Join(sourceRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repoRoot, name), content, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	installScript := filepath.Join(repoRoot, "install-startup.sh")
	uninstallScript := filepath.Join(repoRoot, "uninstall-startup.sh")

	home := t.TempDir()
	fakeBin := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "launchctl-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "launchctl.log")
	bootstrapAttemptsPath := filepath.Join(t.TempDir(), "bootstrap-attempts")
	writeExecutable(t, filepath.Join(fakeBin, "launchctl"), `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FAKE_LAUNCH_LOG"
case "$1" in
  print)
    label=${2##*/}
    test -f "$FAKE_LAUNCH_STATE_DIR/$label"
    ;;
  bootstrap)
    attempts=0
    if test -f "$FAKE_BOOTSTRAP_ATTEMPTS"; then attempts=$(cat "$FAKE_BOOTSTRAP_ATTEMPTS"); fi
    attempts=$((attempts + 1))
    printf '%s\n' "$attempts" > "$FAKE_BOOTSTRAP_ATTEMPTS"
    if test "$attempts" -eq 1; then exit 5; fi
    label=$(basename "$3" .plist)
    : > "$FAKE_LAUNCH_STATE_DIR/$label"
    ;;
  bootout)
    label=${2##*/}
    rm -f "$FAKE_LAUNCH_STATE_DIR/$label"
    ;;
  enable|disable|kickstart) ;;
  *) exit 2 ;;
esac
`)
	writeExecutable(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
printf '{"status":"ok"}\n'
`)
	writeExecutable(t, filepath.Join(fakeBin, "lsof"), `#!/bin/sh
exit 1
`)

	environment := append(os.Environ(),
		"HOME="+home,
		"PATH="+fakeBin+":/usr/bin:/bin:/usr/sbin:/sbin",
		"FAKE_LAUNCH_STATE_DIR="+stateDir,
		"FAKE_LAUNCH_LOG="+logPath,
		"FAKE_BOOTSTRAP_ATTEMPTS="+bootstrapAttemptsPath,
	)
	legacyLabel := "com.tarunjain.agent-history"
	legacyPlist := filepath.Join(home, "Library", "LaunchAgents", legacyLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(legacyPlist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPlist, []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, legacyLabel), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runStartupScript(t, installScript, environment)
	runStartupScript(t, installScript, environment)

	label := "io.github.jaintarun.agent-history"
	plistPath := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	if output, err := exec.Command("plutil", "-lint", plistPath).CombinedOutput(); err != nil {
		t.Fatalf("lint generated plist: %v: %s", err, output)
	}
	assertPlistValue(t, plistPath, "Label", label)
	assertPlistValue(t, plistPath, "RunAtLoad", "true")
	assertPlistValue(t, plistPath, "KeepAlive", "true")
	assertPlistValue(t, plistPath, "ProgramArguments.1", filepath.Join(repoRoot, "run-local.sh"))
	if _, err := os.Stat(legacyPlist); !os.IsNotExist(err) {
		t.Fatalf("legacy plist still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, legacyLabel)); !os.IsNotExist(err) {
		t.Fatalf("legacy service still exists: %v", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := countLaunchctlCommand(string(logData), "bootstrap "); got != 3 {
		t.Fatalf("bootstrap attempt count = %d, want 3; log:\n%s", got, logData)
	}

	runStartupScript(t, uninstallScript, environment)
	runStartupScript(t, uninstallScript, environment)
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Fatalf("plist still exists after uninstall: %v", err)
	}
	if entries, err := os.ReadDir(stateDir); err != nil || len(entries) != 0 {
		t.Fatalf("launch state after uninstall = %v, %v", entries, err)
	}
	logData, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := countLaunchctlCommand(string(logData), "bootout "); got != 3 {
		t.Fatalf("bootout count = %d, want 3; log:\n%s", got, logData)
	}
}

func assertPlistValue(t *testing.T, path, key, want string) {
	t.Helper()
	output, err := exec.Command("plutil", "-extract", key, "raw", "-o", "-", path).CombinedOutput()
	if err != nil {
		t.Fatalf("read plist key %s: %v: %s", key, err, output)
	}
	if got := strings.TrimSpace(string(output)); got != want {
		t.Fatalf("plist %s = %q, want %q", key, got, want)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func runStartupScript(t *testing.T, path string, environment []string) {
	t.Helper()
	command := exec.Command("/bin/sh", path)
	command.Env = environment
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run %s: %v\n%s", filepath.Base(path), err, output)
	}
}

func countLaunchctlCommand(log, prefix string) int {
	count := 0
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, prefix) {
			count++
		}
	}
	return count
}

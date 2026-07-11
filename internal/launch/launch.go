// Package launch resumes trusted stored sessions through cmux or a copyable command.
package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tarunjain/agent-history/internal/source"
	"github.com/tarunjain/agent-history/internal/store"
)

var safeSessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
var safeShellWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// Result describes either a cmux launch or a command the user must copy.
type Result struct {
	Mode      string `json:"mode"`
	Command   string `json:"command,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}

// Launcher builds resume invocations from trusted source adapters.
type Launcher struct {
	store   *store.Store
	sources map[string]source.Source
	runner  runner
}

type runner interface {
	LookPath(string) (string, error)
	Run(context.Context, string, []string) ([]byte, error)
}

// New constructs the production launcher.
func New(database *store.Store, sources ...source.Source) *Launcher {
	return newWithRunner(database, osRunner{}, sources...)
}

func newWithRunner(database *store.Store, commandRunner runner, sources ...source.Source) *Launcher {
	configured := make(map[string]source.Source, len(sources))
	for _, adapter := range sources {
		configured[adapter.Name()] = adapter
	}
	return &Launcher{store: database, sources: configured, runner: commandRunner}
}

// Launch resumes sessionID using mode auto, cmux, or copy.
func (l *Launcher) Launch(ctx context.Context, sessionID, mode string) (Result, error) {
	if mode != "auto" && mode != "cmux" && mode != "copy" {
		return Result{}, fmt.Errorf("unsupported launcher mode %q", mode)
	}
	detail, err := l.store.GetSession(ctx, sessionID)
	if err != nil {
		return Result{}, err
	}
	adapter := l.sources[detail.Session.Agent]
	if adapter == nil {
		return Result{}, fmt.Errorf("source adapter %q is not configured", detail.Session.Agent)
	}
	spec, err := adapter.ResumeSpec(detail.Session)
	if err != nil {
		return Result{}, err
	}
	if err := validateSpec(detail.Session, spec); err != nil {
		return Result{}, err
	}
	command := renderCommand(spec.Executable, spec.Args)
	if mode == "copy" {
		return Result{Mode: "copy", Command: command}, nil
	}
	cmux, err := l.runner.LookPath("cmux")
	if err != nil {
		return Result{Mode: "copy", Command: command}, nil
	}
	name := workspaceName(detail.Session)
	args := []string{
		"workspace", "create", "--name", name, "--cwd", spec.CWD,
		"--command", command, "--focus", "true",
	}
	output, err := l.runner.Run(ctx, cmux, args)
	if err != nil {
		return Result{}, fmt.Errorf("launch cmux workspace: %w", err)
	}
	return Result{Mode: "cmux", Workspace: strings.TrimSpace(string(output))}, nil
}

func validateSpec(session store.Session, spec source.ResumeSpec) error {
	if !safeSessionID.MatchString(session.NativeSessionID) {
		return errors.New("native session ID contains unsupported characters")
	}
	if !filepath.IsAbs(session.WorkingDirectory) {
		return errors.New("session working directory must be an absolute path")
	}
	info, err := os.Stat(session.WorkingDirectory)
	if err != nil {
		return fmt.Errorf("session working directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("session working directory is not a directory")
	}
	wantExecutable := ""
	wantArgs := []string(nil)
	switch session.Agent {
	case "codex":
		wantExecutable = "codex"
		wantArgs = []string{"resume", session.NativeSessionID}
	case "claude":
		wantExecutable = "claude"
		wantArgs = []string{"--resume", session.NativeSessionID}
	default:
		return fmt.Errorf("unsupported source agent %q", session.Agent)
	}
	if spec.Agent != session.Agent || spec.SessionID != session.NativeSessionID || spec.CWD != session.WorkingDirectory || spec.Executable != wantExecutable || !equalStrings(spec.Args, wantArgs) {
		return errors.New("source adapter returned an untrusted resume specification")
	}
	return nil
}

func renderCommand(executable string, args []string) string {
	words := make([]string, 0, len(args)+1)
	words = append(words, shellQuote(executable))
	for _, arg := range args {
		words = append(words, shellQuote(arg))
	}
	return strings.Join(words, " ")
}

func shellQuote(value string) string {
	if safeShellWord.MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func workspaceName(session store.Session) string {
	name := strings.TrimSpace(strings.NewReplacer("\n", " ", "\r", " ").Replace(session.Title))
	if name == "" {
		name = strings.ToUpper(session.Agent[:1]) + session.Agent[1:] + " session"
	}
	runes := []rune(name)
	if len(runes) > 80 {
		name = string(runes[:80])
	}
	return name
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type osRunner struct{}

func (osRunner) LookPath(file string) (string, error) {
	return exec.LookPath(file)
}

func (osRunner) Run(ctx context.Context, executable string, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, executable, args...).CombinedOutput()
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"

	"github.com/tarunjain/agent-history/internal/analyze"
	"github.com/tarunjain/agent-history/internal/config"
	"github.com/tarunjain/agent-history/internal/httpapi"
	"github.com/tarunjain/agent-history/internal/launch"
	"github.com/tarunjain/agent-history/internal/source"
	"github.com/tarunjain/agent-history/internal/source/claude"
	"github.com/tarunjain/agent-history/internal/source/codex"
	"github.com/tarunjain/agent-history/internal/store"
)

var version = "dev"

var defaultAnalysis = analyze.Options{
	Provider: "codex-cli", Model: "gpt-5.4-mini", PromptVersion: "v1",
	NormalizerVersion: "v1", LeafTargetChars: 12_000, RollupFanout: 8,
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(runContext(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, stdout, stderr)
}

func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "agent-history %s\n", version)
		return 0
	case "serve":
		return runServe(ctx, args[1:], stdout, stderr)
	case "scan":
		return runScan(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "agent-history: unknown command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	defaults := config.Defaults()
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stdout)
	bind := flags.String("bind", defaults.Bind, "loopback address and port to listen on")
	databasePath := flags.String("database", defaults.Database, "path to the SQLite database")
	flags.String("config", defaults.Config, "path to the configuration file")
	openBrowserFlag := flags.Bool("open-browser", defaults.OpenBrowser, "open the web application in the default browser")
	noOpen := flags.Bool("no-open", false, "do not open a browser; print the web application URL")
	flags.Duration("scan-interval", defaults.ScanInterval, "interval between transcript scans (0 disables periodic scans)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	resolvedDatabase, err := config.ExpandPath(*databasePath)
	if err != nil {
		fmt.Fprintf(stderr, "agent-history serve: resolve database path: %v\n", err)
		return 1
	}
	database, err := store.Open(ctx, resolvedDatabase)
	if err != nil {
		fmt.Fprintf(stderr, "agent-history serve: %v\n", err)
		return 1
	}

	options, auto, err := analysisSettings(ctx, database)
	if err != nil {
		_ = database.Close()
		fmt.Fprintf(stderr, "agent-history serve: load analysis settings: %v\n", err)
		return 1
	}
	stale, err := database.RecoverAnalysisStates(ctx, auto)
	if err != nil {
		_ = database.Close()
		fmt.Fprintf(stderr, "agent-history serve: recover analysis state: %v\n", err)
		return 1
	}
	codexSource := codex.New(codex.DefaultHome())
	claudeSource := claude.New(claude.DefaultHome())
	scanner := source.NewScanner(database, codexSource, claudeSource)
	launcher := launch.New(database, codexSource, claudeSource)
	engine := analyze.NewEngine(database, map[string]analyze.Analyzer{
		"codex-cli": analyze.NewCodexCLI(""),
	})
	worker := analyze.NewWorker(database, engine, 16)
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	server, err := httpapi.Start(ctx, *bind, httpapi.Config{
		Store: database, Scanner: scanner, Queue: worker, Launcher: launcher, Logger: logger,
		AnalysisDefaults: options,
	})
	if err != nil {
		worker.Close()
		_ = database.Close()
		fmt.Fprintf(stderr, "agent-history serve: %v\n", err)
		return 1
	}
	for _, sessionID := range stale {
		if !auto {
			break
		}
		if _, err := worker.Enqueue(ctx, sessionID, options); err != nil {
			logger.Error("could not requeue stale analysis", "session_id", sessionID, "error", err)
		}
	}

	fmt.Fprintln(stdout, server.URL())
	if *openBrowserFlag && !*noOpen {
		if err := openBrowser(server.URL()); err != nil {
			logger.Warn("could not open browser", "error", err)
		}
	}
	serveErr := <-server.Done()
	worker.Close()
	closeErr := database.Close()
	if serveErr != nil {
		fmt.Fprintf(stderr, "agent-history serve: %v\n", serveErr)
		return 1
	}
	if closeErr != nil {
		fmt.Fprintf(stderr, "agent-history serve: close database: %v\n", closeErr)
		return 1
	}
	return 0
}

func analysisSettings(ctx context.Context, database *store.Store) (analyze.Options, bool, error) {
	options := defaultAnalysis
	if model, ok, err := database.Setting(ctx, "analysis.model"); err != nil {
		return analyze.Options{}, false, err
	} else if ok {
		options.Model = model
	}
	auto := true
	if value, ok, err := database.Setting(ctx, "analysis.auto"); err != nil {
		return analyze.Options{}, false, err
	} else if ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return analyze.Options{}, false, fmt.Errorf("invalid analysis.auto value %q", value)
		}
		auto = parsed
	}
	return options, auto, nil
}

func openBrowser(url string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("automatic browser launch is only supported on macOS")
	}
	return exec.Command("open", url).Start()
}

func runScan(args []string, stdout, stderr io.Writer) int {
	defaults := config.Defaults()
	flags := flag.NewFlagSet("scan", flag.ContinueOnError)
	flags.SetOutput(stdout)
	agent := flags.String("agent", "all", "transcript source to scan: all, codex, or claude")
	databasePath := flags.String("database", defaults.Database, "path to the SQLite database")
	flags.String("config", defaults.Config, "path to the configuration file")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	resolvedDatabase, err := config.ExpandPath(*databasePath)
	if err != nil {
		fmt.Fprintf(stderr, "agent-history scan: resolve database path: %v\n", err)
		return 1
	}
	database, err := store.Open(context.Background(), resolvedDatabase)
	if err != nil {
		fmt.Fprintf(stderr, "agent-history scan: %v\n", err)
		return 1
	}
	defer database.Close()

	scanner := source.NewScanner(database,
		codex.New(codex.DefaultHome()),
		claude.New(claude.DefaultHome()),
	)
	report, err := scanner.Scan(context.Background(), *agent)
	if err != nil {
		fmt.Fprintf(stderr, "agent-history scan: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "discovered=%d imported=%d metadata_only=%d skipped=%d\n",
		report.Discovered, report.Imported, report.MetadataOnly, report.Skipped)
	return 0
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: agent-history <serve|scan|version> [options]")
}

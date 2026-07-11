package main

import (
	"context"
	"encoding/json"
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
	"strings"
	"syscall"
	"time"

	"github.com/tarunjain/agent-history/internal/analyze"
	"github.com/tarunjain/agent-history/internal/config"
	"github.com/tarunjain/agent-history/internal/evaluation"
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
	NormalizerVersion: "v2", LeafTargetChars: 48_000, RollupFanout: 8,
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
		return runScan(ctx, args[1:], stdout, stderr)
	case "eval":
		return runEval(ctx, args[1:], stdout, stderr)
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
	configPath := flags.String("config", defaults.Config, "path to the configuration file")
	openBrowserFlag := flags.Bool("open-browser", defaults.OpenBrowser, "open the web application in the default browser")
	noOpen := flags.Bool("no-open", false, "do not open a browser; print the web application URL")
	scanOnStart := flags.Bool("scan-on-start", true, "scan transcript sources when the server starts")
	scanInterval := flags.Duration("scan-interval", defaults.ScanInterval, "interval between transcript scans (0 disables periodic scans)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	resolvedConfig, err := config.ExpandPath(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "agent-history serve: resolve config path: %v\n", err)
		return 1
	}
	fileConfig, err := config.Load(resolvedConfig)
	if err != nil {
		fmt.Fprintf(stderr, "agent-history serve: %v\n", err)
		return 1
	}
	configuredAnalysis := defaultAnalysis
	configuredAuto := true
	if fileConfig.Analysis.Provider != "" {
		configuredAnalysis.Provider = fileConfig.Analysis.Provider
	}
	if fileConfig.Analysis.Model != "" {
		configuredAnalysis.Model = fileConfig.Analysis.Model
	}
	if fileConfig.Analysis.Auto != nil {
		configuredAuto = *fileConfig.Analysis.Auto
	}
	if configuredAnalysis.Provider != "codex-cli" || strings.TrimSpace(configuredAnalysis.Model) == "" || len(configuredAnalysis.Model) > 200 {
		fmt.Fprintln(stderr, "agent-history serve: config analysis provider/model is not supported")
		return 1
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
	if err := seedAnalysisSettings(ctx, database, configuredAnalysis, configuredAuto); err != nil {
		_ = database.Close()
		fmt.Fprintf(stderr, "agent-history serve: seed analysis settings: %v\n", err)
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
	worker := analyze.NewWorker(database, engine, 256)
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
	scanContext, stopScans := context.WithCancel(ctx)
	scanDone := startScanLoop(scanContext, scanner, *scanOnStart, *scanInterval, logger)
	if *openBrowserFlag && !*noOpen {
		if err := openBrowser(server.URL()); err != nil {
			logger.Warn("could not open browser", "error", err)
		}
	}
	serveErr := <-server.Done()
	stopScans()
	<-scanDone
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

func seedAnalysisSettings(ctx context.Context, database *store.Store, options analyze.Options, auto bool) error {
	defaults := map[string]string{
		"analysis.provider": options.Provider,
		"analysis.model":    options.Model,
		"analysis.auto":     strconv.FormatBool(auto),
	}
	missing := make(map[string]string)
	for key, value := range defaults {
		if _, ok, err := database.Setting(ctx, key); err != nil {
			return err
		} else if !ok {
			missing[key] = value
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return database.SetSettings(ctx, missing)
}

type scanService interface {
	Scan(context.Context, string) (source.ScanReport, error)
}

func startScanLoop(ctx context.Context, scanner scanService, startup bool, interval time.Duration, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		scan := func() {
			started := time.Now()
			report, err := scanner.Scan(ctx, "all")
			if err != nil {
				if ctx.Err() == nil && logger != nil {
					logger.Error("history scan failed", "error", err, "duration_ms", time.Since(started).Milliseconds())
				}
				return
			}
			if logger != nil {
				logger.Info("history scan complete", "discovered", report.Discovered, "imported", report.Imported, "metadata_only", report.MetadataOnly, "skipped", report.Skipped, "duration_ms", time.Since(started).Milliseconds())
			}
		}
		if startup {
			scan()
		}
		if interval <= 0 {
			<-ctx.Done()
			return
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				scan()
			}
		}
	}()
	return done
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

func runEval(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("eval", flag.ContinueOnError)
	flags.SetOutput(stdout)
	model := flags.String("model", defaultAnalysis.Model, "Codex model used for evaluation")
	executable := flags.String("codex", "codex", "path to the Codex CLI executable")
	allowUsage := flags.Bool("allow-provider-usage", false, "acknowledge that evaluation consumes Codex subscription usage")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if !*allowUsage {
		fmt.Fprintln(stderr, "agent-history eval: --allow-provider-usage is required because evaluation invokes Codex repeatedly")
		return 2
	}
	report, err := evaluation.Run(ctx, analyze.NewCodexCLI(*executable), *model)
	if err != nil {
		fmt.Fprintf(stderr, "agent-history eval: %v\n", err)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		fmt.Fprintf(stderr, "agent-history eval: encode report: %v\n", err)
		return 1
	}
	if !report.Passed {
		return 1
	}
	return 0
}

func runScan(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	defaults := config.Defaults()
	flags := flag.NewFlagSet("scan", flag.ContinueOnError)
	flags.SetOutput(stdout)
	agent := flags.String("agent", "all", "transcript source to scan: all, codex, or claude")
	databasePath := flags.String("database", defaults.Database, "path to the SQLite database")
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
	database, err := store.Open(ctx, resolvedDatabase)
	if err != nil {
		fmt.Fprintf(stderr, "agent-history scan: %v\n", err)
		return 1
	}
	defer database.Close()

	scanner := source.NewScanner(database,
		codex.New(codex.DefaultHome()),
		claude.New(claude.DefaultHome()),
	)
	report, err := scanner.Scan(ctx, *agent)
	if err != nil {
		fmt.Fprintf(stderr, "agent-history scan: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "discovered=%d imported=%d metadata_only=%d skipped=%d\n",
		report.Discovered, report.Imported, report.MetadataOnly, report.Skipped)
	return 0
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: agent-history <serve|scan|eval|version> [options]")
}

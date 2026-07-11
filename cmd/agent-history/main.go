package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/tarunjain/agent-history/internal/config"
	"github.com/tarunjain/agent-history/internal/source"
	"github.com/tarunjain/agent-history/internal/source/codex"
	"github.com/tarunjain/agent-history/internal/store"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "agent-history %s\n", version)
		return 0
	case "serve":
		return runServe(args[1:], stdout, stderr)
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

func runServe(args []string, stdout, stderr io.Writer) int {
	defaults := config.Defaults()
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stdout)
	flags.String("bind", defaults.Bind, "loopback address and port to listen on")
	flags.String("database", defaults.Database, "path to the SQLite database")
	flags.String("config", defaults.Config, "path to the configuration file")
	flags.Bool("open-browser", defaults.OpenBrowser, "open the web application in the default browser")
	flags.Bool("no-open", false, "do not open a browser; print the web application URL")
	flags.Duration("scan-interval", defaults.ScanInterval, "interval between transcript scans (0 disables periodic scans)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	fmt.Fprintln(stderr, "agent-history serve: not implemented")
	return 1
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

	scanner := source.NewScanner(database, codex.New(codex.DefaultHome()))
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

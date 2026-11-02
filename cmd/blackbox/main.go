// Command blackbox is Spark's private log store. One binary runs as each part
// of Blackbox or as its CLI, picked by the first argument.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata" // TZ works on an image without zoneinfo

	"github.com/lsariol/blackbox/internal/cli"
	"github.com/lsariol/blackbox/internal/config"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		printUsage(os.Stdout)
		return
	}

	switch {
	case args[0] == "-h" || args[0] == "--help":
		printUsage(os.Stdout)
	case args[0] == "shell":
		runShell()
	case args[0] == "--version":
		os.Exit(runCommand([]string{"version"}))
	default:
		os.Exit(runCommand(args))
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  blackbox shell                         Open the CLI
  blackbox <command> [args]              Run one CLI command and exit
  blackbox proxy | collector | api       Run that part of Blackbox (what the compose file starts)
  blackbox health <proxy|collector|api>  Exit 0 if that part is healthy (the healthchecks)

In Docker: docker exec -it blackbox-api /blackbox shell
       or: docker exec blackbox-api /blackbox help
Run "blackbox help" to list the CLI commands.
`)
}

// runShell runs the interactive CLI until exit, end of input, or SIGTERM.
func runShell() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	cli.New(cliOptions(loadConfig())).Run(ctx)
}

// runCommand runs one CLI command and returns the exit status.
func runCommand(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.New(cliOptions(loadConfig())).Exec(ctx, args); err != nil {
		cli.Report(err)
		return 1
	}
	return 0
}

// cliOptions are the CLI's settings.
func cliOptions(cfg config.Config) cli.Options {
	return cli.Options{Env: cfg.Env, Version: cfg.Version}
}

func loadConfig() config.Config {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fatal(err)
	}
	return cfg
}

// fatal prints an error and exits.
func fatal(err error) {
	fmt.Fprintf(os.Stderr, "blackbox: %v\n", err)
	os.Exit(1)
}

// Command blackbox is Spark's private log store. One binary runs as each part
// of Blackbox or as its CLI, picked by the first argument.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"
	_ "time/tzdata" // TZ works on an image without zoneinfo

	"github.com/lsariol/blackbox/internal/cli"
	"github.com/lsariol/blackbox/internal/collector"
	"github.com/lsariol/blackbox/internal/config"
	"github.com/lsariol/blackbox/internal/containers"
	"github.com/lsariol/blackbox/internal/proxy"
	"github.com/lsariol/blackbox/internal/store"
)

// roles are the parts of Blackbox the compose file starts, one per container.
var roles = []string{"proxy", "collector", "api"}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		printUsage(os.Stdout)
		return
	}

	switch {
	case args[0] == "-h" || args[0] == "--help":
		printUsage(os.Stdout)
	case args[0] == "proxy":
		runProxy()
	case args[0] == "collector":
		runCollector()
	case args[0] == "health":
		os.Exit(runHealth(args[1:]))
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

// runProxy runs blackbox-proxy until SIGINT or SIGTERM (docker stop).
func runProxy() {
	cfg := loadConfig()
	log := newLogger(cfg, "proxy")
	log.Info("Blackbox starting", "version", cfg.Version, "env", cfg.Env)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := proxy.New(proxy.DockerSocket, log).Serve(ctx, containers.Socket); err != nil {
		fatal(err)
	}
	log.Info("Blackbox stopped")
}

// runCollector runs blackbox-collector until SIGINT or SIGTERM (docker stop).
func runCollector() {
	cfg := loadConfig()
	log := newLogger(cfg, "collector")
	log.Info("Blackbox starting", "version", cfg.Version, "env", cfg.Env)

	migratorURL, err := config.Secret(os.Getenv, "BLACKBOX_DATABASE_MIGRATOR_URL")
	if err != nil {
		fatal(err)
	}
	appURL, err := config.Secret(os.Getenv, "BLACKBOX_DATABASE_URL")
	if err != nil {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := collector.New(collector.Options{
		Version: cfg.Version,
		Docker:  containers.NewClient(containers.Socket),
		Log:     log,
		OpenStore: func(ctx context.Context) (collector.Store, error) {
			if err := store.Migrate(ctx, migratorURL, log); err != nil {
				return nil, err
			}
			db, err := store.Connect(ctx, appURL, "BLACKBOX_DATABASE_URL")
			if err != nil {
				return nil, err
			}
			if err := db.CheckSchemaVersion(ctx); err != nil {
				db.Close()
				return nil, err
			}
			log.Info("database ready")
			return db, nil
		},
	})
	if err := c.Run(ctx); err != nil {
		fatal(err)
	}
	log.Info("Blackbox stopped")
}

// runHealth is a container's healthcheck: 0 when the named part is healthy.
func runHealth(args []string) int {
	if len(args) != 1 || !slices.Contains(roles, args[0]) {
		fmt.Fprintln(os.Stderr, "! Usage: blackbox health <proxy|collector|api>")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var err error
	switch args[0] {
	case "proxy":
		err = containers.NewClient(containers.Socket).Healthy(ctx)
	case "collector":
		err = collector.Healthy(collector.HeartbeatFile, time.Now())
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "blackbox health: %v\n", err)
		return 1
	}
	fmt.Println("healthy")
	return 0
}

// newLogger logs to stderr, as Spark's logging standard asks: JSON records with
// UTC times, or text when APP_ENV=dev, tagged with the part they come from.
func newLogger(cfg config.Config, part string) *slog.Logger {
	opts := &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				a.Value = slog.TimeValue(a.Value.Time().UTC())
			}
			return a
		},
	}
	var h slog.Handler = slog.NewJSONHandler(os.Stderr, opts)
	if cfg.Env == "dev" {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h).With("component", part)
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

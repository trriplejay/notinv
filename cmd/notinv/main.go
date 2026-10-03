// Command notinv runs scheduled website checks, records their results, and
// sends Discord notifications when a check's state changes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/trriplejay/notinv/internal/config"
	"github.com/trriplejay/notinv/internal/runner"
	"github.com/trriplejay/notinv/internal/store"
	"github.com/trriplejay/notinv/scripts/example"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	showVersion := flag.Bool("version", false, "print version and exit")
	envFile := flag.String("env-file", ".env", "dotenv file to load")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	explicitEnvFile := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "env-file" {
			explicitEnvFile = true
		}
	})
	if err := loadEnvFile(*envFile, explicitEnvFile); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	warnIfDryRun(slog.Default(), cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.DatabaseAuthToken)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	logger := slog.Default()
	httpClient := &http.Client{Timeout: 30 * time.Second}
	rc := runner.NewContext(logger, httpClient)

	scripts := []runner.Script{example.New()}
	var wg sync.WaitGroup
	for _, script := range scripts {
		wg.Add(1)
		go func(s runner.Script) {
			defer wg.Done()
			if err := runner.Run(ctx, s, rc, st); err != nil {
				slog.Error("runner exited", "script", s.Name(), "err", err)
			}
		}(script)
	}

	slog.Info("notinv starting", "version", version)

	<-ctx.Done()

	slog.Info("notinv shutting down")
	wg.Wait()
	return nil
}

func loadEnvFile(path string, explicit bool) error {
	if err := godotenv.Load(path); err != nil {
		if !explicit && path == ".env" && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("load dotenv file %q: %w", path, err)
	}
	return nil
}

func warnIfDryRun(logger *slog.Logger, cfg *config.Config) {
	if cfg.DiscordDryRun {
		logger.Warn("Discord notifications are running in dry-run mode")
	}
}

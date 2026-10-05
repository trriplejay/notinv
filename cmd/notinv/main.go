// Command notinv runs scheduled website checks, records their results, and
// sends Discord notifications when a check's state changes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/trriplejay/notinv/internal/config"
	"github.com/trriplejay/notinv/internal/store"
)

const retentionInterval = 24 * time.Hour

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() (retErr error) {
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
	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
		retErr = errors.Join(retErr, st.Close())
	}()

	slog.Info("notinv starting", "version", version)

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(retentionInterval)
		defer ticker.Stop()

		for {
			// Sweep at startup and after each tick. Recheck cancellation because
			// select may choose a ready tick even when ctx.Done is also ready.
			if ctx.Err() != nil {
				return
			}
			cutoff := time.Now().AddDate(0, 0, -cfg.RetentionDays)
			if err := st.DeleteOlderThan(ctx, cutoff); err != nil {
				slog.Error("retention delete failed", "err", err)
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	<-ctx.Done()

	slog.Info("notinv shutting down")
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

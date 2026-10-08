// Command notinv runs scheduled website checks, records their results, and
// sends Discord notifications when a check's state changes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/trriplejay/notinv/internal/config"
	"github.com/trriplejay/notinv/internal/rc"
	"github.com/trriplejay/notinv/internal/runner"
	"github.com/trriplejay/notinv/internal/store"
	"github.com/trriplejay/notinv/internal/web"
	"github.com/trriplejay/notinv/scripts/example"
	"github.com/trriplejay/notinv/scripts/nintendo"
	webassets "github.com/trriplejay/notinv/web"
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
	logger := newLogger(os.Stderr, cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)
	warnIfDryRun(logger, cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.DatabaseAuthToken)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	writer := rc.NewWriter(st, version, logger, rc.Options{})

	httpClient := &http.Client{Timeout: 30 * time.Second}
	services := runner.NewContext(logger, httpClient, cfg)
	scripts := defaultScripts()
	cancelRuns, drainDone := startRunners(ctx, scripts, services, st)
	defer cancelRuns()

	schedules := make(map[string]string, len(scripts))
	for _, script := range scripts {
		schedules[script.Name()] = script.Schedule()
	}

	mux := newMux(st, schedules)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	slog.Info("notinv starting", "version", version)

	var wg sync.WaitGroup
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
			if err := st.DeleteOlderThan(ctx, cutoff); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("retention delete failed", "err", err)
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-serverErr:
		if err != nil {
			serveErr = fmt.Errorf("serve: %w", err)
		}
		// Stop scheduling and retention even when startup, not a signal, fails.
		stop()
	}

	shutdownErr := orchestrateShutdown(defaultShutdownBudget(), drainDone, cancelRuns,
		srv.Shutdown, func() error {
			wg.Wait()
			writer.Close()
			return st.Close()
		}, logger)
	return errors.Join(serveErr, shutdownErr)
}

// defaultScripts returns the scripts scheduled by the service.
func defaultScripts() []runner.Script {
	return []runner.Script{example.New(), nintendo.New()}
}

func newLogger(w io.Writer, level, format string) *slog.Logger {
	logLevel := slog.LevelInfo
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: logLevel}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

type shutdownBudget struct {
	overall time.Duration
	drain   time.Duration
}

func defaultShutdownBudget() shutdownBudget {
	return shutdownBudget{overall: 8 * time.Second, drain: 5 * time.Second}
}

func (b shutdownBudget) httpShutdown(elapsed time.Duration) time.Duration {
	return max(0, b.overall-elapsed)
}

// The scheduler stops on the signal, but an active script and its final result
// write retain their own context until the drain resolves.
type drainingScript struct {
	runner.Script
	ctx context.Context
}

func (s drainingScript) Run(_ context.Context, services *runner.Context) error {
	return s.Script.Run(s.ctx, services)
}

type drainingStore struct {
	runner.RunStore
	ctx context.Context
}

func (s drainingStore) InsertRun(_ context.Context, run store.Run) error {
	return s.RunStore.InsertRun(s.ctx, run)
}

func startRunners(ctx context.Context, scripts []runner.Script, services *runner.Context, st runner.RunStore) (context.CancelFunc, <-chan struct{}) {
	runCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, script := range scripts {
		wg.Add(1)
		go func(s runner.Script) {
			defer wg.Done()
			if err := runner.Run(ctx, drainingScript{Script: s, ctx: runCtx}, services,
				drainingStore{RunStore: st, ctx: runCtx}); err != nil {
				services.Log.Error("runner exited", "script", s.Name(), "err", err)
			}
		}(script)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return cancel, done
}

// HTTP gets only the time left after draining, not a fresh independent budget.
// Errors are returned for the caller to log once; ErrServerClosed is normal.
func orchestrateShutdown(budget shutdownBudget, drainDone <-chan struct{}, cancelRuns context.CancelFunc,
	httpShutdown func(context.Context) error, dbClose func() error, logger *slog.Logger,
) error {
	started := time.Now()
	logger.Info("notinv shutting down")
	timer := time.NewTimer(min(budget.drain, budget.overall))
	defer timer.Stop()
	select {
	case <-drainDone:
	case <-timer.C:
	}
	cancelRuns()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), budget.httpShutdown(time.Since(started)))
	defer cancel()
	httpErr := httpShutdown(shutdownCtx)
	if errors.Is(httpErr, http.ErrServerClosed) {
		httpErr = nil
	}
	return errors.Join(httpErr, dbClose())
}

func newMux(st *store.Store, schedules map[string]string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /", webassets.Handler())
	mux.Handle("GET /api/scripts", web.NewScriptsHandler(st, schedules, nil))
	mux.Handle("GET /api/scripts/{name}/runs", web.NewRunsHandler(st, nil))
	mux.Handle("GET /api/scripts/{name}/requests", web.NewRequestsHandler(st, nil))
	mux.Handle("GET /api/scripts/{name}/requests/series", web.NewSeriesHandler(st, nil))
	mux.Handle("GET /healthz", web.NewHealthHandler(st))
	return mux
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

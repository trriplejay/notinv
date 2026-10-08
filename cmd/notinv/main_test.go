package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/joho/godotenv"

	"github.com/trriplejay/notinv/internal/config"
	"github.com/trriplejay/notinv/internal/rc"
	"github.com/trriplejay/notinv/internal/runner"
	"github.com/trriplejay/notinv/internal/store"
)

// TestDefaultScripts verifies that the service's runnable set includes Nintendo.
func TestDefaultScripts(t *testing.T) {
	for _, script := range defaultScripts() {
		if script.Name() == "nintendo" {
			if script.Schedule() != "@every 1m" {
				t.Fatalf("nintendo schedule = %q, want @every 1m", script.Schedule())
			}
			return
		}
	}
	t.Fatal("nintendo is absent from the service's default scripts")
}

// CLM-11: an absent implicit .env must not change the environment.
func TestLoadEnvFileDefaultMissing(t *testing.T) {
	t.Chdir(t.TempDir())
	before := os.Environ()
	if err := loadEnvFile(".env", false); err != nil {
		t.Fatalf("loadEnvFile: %v", err)
	}
	if !reflect.DeepEqual(os.Environ(), before) {
		t.Error("missing default dotenv file changed the environment")
	}
}

// CLM-11: explicitly requesting even the default path makes it required.
func TestLoadEnvFileExplicitMissing(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, path := range []string{".env", "custom.env"} {
		t.Run(path, func(t *testing.T) {
			err := loadEnvFile(path, true)
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("expected wrapped missing-file error, got %v", err)
			}
		})
	}
}

// CLM-11: existing files load variables whether implicit or explicit.
func TestLoadEnvFileExisting(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "implicit"
		if explicit {
			name = "explicit"
		}
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			const key = "NOTINV_TEST_DOTENV_VALUE"
			// Register cleanup before unsetting so godotenv can populate the key.
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatalf("unset test variable: %v", err)
			}
			if err := os.WriteFile(".env", []byte(key+"=fromfile\n"), 0o600); err != nil {
				t.Fatalf("write dotenv file: %v", err)
			}
			if err := loadEnvFile(".env", explicit); err != nil {
				t.Fatalf("loadEnvFile: %v", err)
			}
			if got := os.Getenv(key); got != "fromfile" {
				t.Errorf("dotenv value = %q, want fromfile", got)
			}
		})
	}
}

func TestLoadEnvFileInvalid(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("NOTINV_TEST_DOTENV_VALUE=\"unterminated\n"), 0o600); err != nil {
		t.Fatalf("write dotenv file: %v", err)
	}
	if err := loadEnvFile(".env", false); err == nil {
		t.Fatal("expected malformed default dotenv file to fail")
	}
}

// CLM-12: real environment values, including empty ones, take precedence.
func TestLoadEnvFileRealEnvironmentWins(t *testing.T) {
	for _, value := range []string{"real", ""} {
		t.Run("value="+value, func(t *testing.T) {
			const key = "NOTINV_TEST_DOTENV_VALUE"
			t.Setenv(key, value)
			path := filepath.Join(t.TempDir(), "custom.env")
			if err := os.WriteFile(path, []byte(key+"=fromfile\n"), 0o600); err != nil {
				t.Fatalf("write dotenv file: %v", err)
			}
			if err := loadEnvFile(path, true); err != nil {
				t.Fatalf("loadEnvFile: %v", err)
			}
			if got := os.Getenv(key); got != value {
				t.Errorf("environment value = %q, want %q", got, value)
			}
		})
	}
}

// CLM-7: dry-run notifications warn at startup; configured notifications do not.
func TestWarnIfDryRun(t *testing.T) {
	for _, tt := range []struct {
		name   string
		dryRun bool
	}{
		{name: "dry-run", dryRun: true},
		{name: "configured", dryRun: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&output, nil))
			warnIfDryRun(logger, &config.Config{DiscordDryRun: tt.dryRun})
			if !tt.dryRun {
				if output.Len() != 0 {
					t.Errorf("unexpected log: %s", output.String())
				}
				return
			}
			for _, want := range []string{"level=WARN", "notifications", "dry-run"} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("log %q does not contain %q", output.String(), want)
				}
			}
		})
	}
}

// CLM-13: the repository's example is valid dotenv with all eight settings.
func TestEnvExampleParses(t *testing.T) {
	values, err := godotenv.Read("../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	keys := []string{
		"NOTINV_LISTEN",
		"DATABASE_URL",
		"NOTINV_RETENTION_DAYS",
		"NOTINV_LOG_LEVEL",
		"NOTINV_LOG_FORMAT",
		"DATABASE_AUTH_TOKEN",
		"NOTINV_DISCORD_TOKEN",
		"NOTINV_DISCORD_USER_ID",
	}
	if len(values) != len(keys) {
		t.Errorf("example has %d variables, want %d", len(values), len(keys))
	}
	for _, key := range keys {
		if _, ok := values[key]; !ok {
			t.Errorf("example missing %s", key)
		}
	}
}

// CLM-2, CLM-3: every validated level and format changes emitted records.
func TestNewLogger(t *testing.T) {
	levels := []struct {
		name  string
		level slog.Level
	}{
		{"debug", slog.LevelDebug}, {"info", slog.LevelInfo},
		{"warn", slog.LevelWarn}, {"error", slog.LevelError},
	}
	for _, format := range []string{"text", "json"} {
		for _, configured := range levels {
			t.Run(format+"/"+configured.name, func(t *testing.T) {
				var output bytes.Buffer
				logger := newLogger(&output, configured.name, format)
				for _, record := range levels {
					output.Reset()
					logger.Log(context.Background(), record.level, "record")
					if record.level < configured.level {
						if output.Len() != 0 {
							t.Errorf("%s was not filtered: %s", record.name, output.String())
						}
						continue
					}
					if format == "json" {
						var got map[string]any
						if err := json.Unmarshal(output.Bytes(), &got); err != nil {
							t.Fatalf("invalid JSON: %v", err)
						}
						if got["level"] != record.level.String() || got["msg"] != "record" {
							t.Errorf("unexpected record: %v", got)
						}
					} else if json.Valid(output.Bytes()) || !strings.Contains(output.String(), "level="+record.level.String()) {
						t.Errorf("expected text record, got %q", output.String())
					}
				}
			})
		}
	}
}

// CLM-6, CLM-7, CLM-8: neither cancellation nor teardown jumps the drain.
func TestOrchestrateShutdownDrain(t *testing.T) {
	for _, completes := range []bool{true, false} {
		name := "timeout"
		if completes {
			name = "completion"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				budget := shutdownBudget{overall: 80 * time.Millisecond, drain: 50 * time.Millisecond}
				drainDone := make(chan struct{})
				finished := make(chan error, 1)
				var order []string
				var mu sync.Mutex
				record := func(step string) {
					mu.Lock()
					defer mu.Unlock()
					order = append(order, step)
				}
				var output bytes.Buffer
				started := time.Now()
				go func() {
					finished <- orchestrateShutdown(budget, drainDone,
						func() { record("cancel") },
						func(ctx context.Context) error {
							record("http")
							deadline, ok := ctx.Deadline()
							if !ok || !deadline.Equal(started.Add(budget.overall)) {
								t.Errorf("HTTP deadline = %v, want shared deadline %v", deadline, started.Add(budget.overall))
							}
							return nil
						}, func() error {
							record("db")
							return nil
						}, newLogger(&output, "debug", "text"))
				}()
				synctest.Wait()
				time.Sleep(10 * time.Millisecond)
				synctest.Wait()
				mu.Lock()
				premature := append([]string(nil), order...)
				mu.Unlock()
				if len(premature) != 0 {
					t.Fatalf("shutdown preceded drain: %v", premature)
				}
				wantElapsed := budget.drain
				if completes {
					record("completed")
					close(drainDone)
					wantElapsed = 10 * time.Millisecond
				}
				if err := <-finished; err != nil {
					t.Fatalf("shutdown: %v", err)
				}
				if elapsed := time.Since(started); elapsed != wantElapsed {
					t.Errorf("drain elapsed %v, want %v", elapsed, wantElapsed)
				}
				want := []string{"cancel", "http", "db"}
				if completes {
					want = append([]string{"completed"}, want...)
				}
				if !reflect.DeepEqual(order, want) {
					t.Errorf("order = %v, want %v", order, want)
				}
				if strings.Contains(output.String(), "level=ERROR") {
					t.Errorf("clean shutdown logged error: %s", output.String())
				}
			})
		})
	}
}

func TestShutdownBudget(t *testing.T) {
	budget := defaultShutdownBudget()
	if budget.overall != 8*time.Second || budget.drain != 5*time.Second {
		t.Fatalf("production budget = %+v", budget)
	}
	if total := budget.drain + budget.httpShutdown(budget.drain); total > budget.overall || total >= 10*time.Second {
		t.Errorf("shutdown timeouts total %v", total)
	}
	for _, b := range []shutdownBudget{budget, {overall: 90 * time.Millisecond, drain: 20 * time.Millisecond}} {
		for _, elapsed := range []time.Duration{0, b.drain, b.overall, b.overall + time.Second} {
			want := b.overall - elapsed
			if want < 0 {
				want = 0
			}
			if got := b.httpShutdown(elapsed); got != want {
				t.Errorf("remaining HTTP budget = %v, want %v", got, want)
			}
		}
	}
}

type shutdownScript struct {
	run func(context.Context) error
}

func (s shutdownScript) Name() string                                     { return "shutdown-test" }
func (s shutdownScript) Schedule() string                                 { return "@every 1s" }
func (s shutdownScript) Run(ctx context.Context, _ *runner.Context) error { return s.run(ctx) }

func staticServices(services *runner.Context) func(runner.Script) *runner.Context {
	return func(runner.Script) *runner.Context { return services }
}

type shutdownRunStore struct {
	insert func(context.Context, store.Run) error
}

func (s shutdownRunStore) InsertRun(ctx context.Context, run store.Run) error {
	return s.insert(ctx, run)
}

// CLM-1, CLM-2, CLM-6: exercise the production scheduling/context adapter,
// including persistence after a simulated signal, not just a fake drain channel.
func TestShutdownInFlightRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var output bytes.Buffer
		logger := newLogger(&output, "debug", "text")
		ctx, signalStop := context.WithCancel(context.Background())
		defer signalStop()
		started := make(chan context.Context, 1)
		release := make(chan struct{})
		completed := false
		recorded := false
		script := shutdownScript{run: func(ctx context.Context) error {
			started <- ctx
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
				completed = true
				return nil
			}
		}}
		st := shutdownRunStore{insert: func(ctx context.Context, run store.Run) error {
			if ctx.Err() != nil || !run.OK {
				t.Errorf("completion lost: context=%v run=%+v", ctx.Err(), run)
			}
			recorded = true
			return nil
		}}
		cancelRuns, done := startRunners(ctx, []runner.Script{script}, staticServices(&runner.Context{Log: logger}), st)
		defer cancelRuns()
		runCtx := <-started
		signalStop()
		synctest.Wait()
		if runCtx.Err() != nil {
			t.Fatal("signal cancelled the active run")
		}
		finished := make(chan error, 1)
		var order []string
		go func() {
			finished <- orchestrateShutdown(shutdownBudget{overall: 80 * time.Millisecond, drain: 50 * time.Millisecond},
				done, cancelRuns, func(ctx context.Context) error {
					if _, ok := ctx.Deadline(); !ok {
						t.Error("HTTP shutdown has no timeout")
					}
					if !completed || !recorded {
						t.Error("HTTP shutdown preceded normal run completion/persistence")
					}
					if runCtx.Err() == nil {
						t.Error("run context not cancelled after drain")
					}
					order = append(order, "http")
					return nil
				}, func() error {
					order = append(order, "db")
					return nil
				}, logger)
		}()
		synctest.Wait()
		time.Sleep(10 * time.Millisecond)
		close(release)
		if err := <-finished; err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		if !reflect.DeepEqual(order, []string{"http", "db"}) {
			t.Errorf("order = %v", order)
		}
		if strings.Contains(output.String(), "level=ERROR") {
			t.Errorf("clean stop logged error: %s", output.String())
		}
	})
}

func TestOrchestrateShutdownErrors(t *testing.T) {
	httpFailure := errors.New("HTTP shutdown failed")
	dbFailure := errors.New("DB close failed")
	for _, httpErr := range []error{nil, http.ErrServerClosed, httpFailure} {
		var output bytes.Buffer
		done := make(chan struct{})
		close(done)
		err := orchestrateShutdown(defaultShutdownBudget(), done, func() {},
			func(context.Context) error { return httpErr }, func() error { return dbFailure },
			newLogger(&output, "debug", "text"))
		if !errors.Is(err, dbFailure) {
			t.Errorf("DB error lost: %v", err)
		}
		if errors.Is(err, httpFailure) != errors.Is(httpErr, httpFailure) {
			t.Errorf("HTTP error lost: %v", err)
		}
		if errors.Is(err, http.ErrServerClosed) {
			t.Errorf("normal server close returned as error: %v", err)
		}
	}
}

func TestShutdownIdleRunners(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, signalStop := context.WithCancel(context.Background())
		defer signalStop()
		var output bytes.Buffer
		logger := newLogger(&output, "debug", "text")
		var calls atomic.Int32
		script := shutdownScript{run: func(context.Context) error {
			calls.Add(1)
			return nil
		}}
		st := shutdownRunStore{insert: func(context.Context, store.Run) error { return nil }}
		cancelRuns, done := startRunners(ctx, []runner.Script{script}, staticServices(&runner.Context{Log: logger}), st)
		defer cancelRuns()
		synctest.Wait() // The startup run has finished; the runner is idle until its next fire.
		signalStop()
		defer func() {
			if n := calls.Load(); n != 1 {
				t.Errorf("script ran %d times, want only the startup run", n)
			}
		}()
		started := time.Now()
		if err := orchestrateShutdown(defaultShutdownBudget(), done, cancelRuns,
			func(context.Context) error { return nil }, func() error { return nil }, logger); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		if elapsed := time.Since(started); elapsed != 0 {
			t.Errorf("idle runners consumed drain window: %v", elapsed)
		}
	})
}

func TestShutdownCancelsRunAtDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, signalStop := context.WithCancel(context.Background())
		defer signalStop()
		var output bytes.Buffer
		logger := newLogger(&output, "debug", "text")
		started := make(chan context.Context, 1)
		script := shutdownScript{run: func(ctx context.Context) error {
			started <- ctx
			<-ctx.Done()
			return ctx.Err()
		}}
		st := shutdownRunStore{insert: func(context.Context, store.Run) error { return nil }}
		cancelRuns, done := startRunners(ctx, []runner.Script{script}, staticServices(&runner.Context{Log: logger}), st)
		defer cancelRuns()
		runCtx := <-started
		signalStop()
		budget := shutdownBudget{overall: 80 * time.Millisecond, drain: 50 * time.Millisecond}
		start := time.Now()
		if err := orchestrateShutdown(budget, done, cancelRuns, func(context.Context) error {
			if runCtx.Err() == nil {
				t.Error("overdue run was not cancelled before HTTP shutdown")
			}
			return nil
		}, func() error { return nil }, logger); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		if elapsed := time.Since(start); elapsed != budget.drain {
			t.Errorf("run cancelled after %v, want %v", elapsed, budget.drain)
		}
		<-done
	})
}

// Script requests must be recorded so the dashboard can chart them.
func TestServicesRecordScriptRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer server.Close()
	st, err := store.Open(t.Context(), "file:"+filepath.Join(t.TempDir(), "test.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logger := newLogger(&bytes.Buffer{}, "info", "text")
	writer := rc.NewWriter(st, "test", logger, rc.Options{})
	services := newServicesFor(logger, &config.Config{DiscordDryRun: true}, writer)(shutdownScript{})
	resp, err := services.HTTP.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	writer.Close() // Flushes buffered records.
	requests, err := st.QueryRequests(t.Context(), "shutdown-test", time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].URL != server.URL || requests[0].StatusCode != http.StatusTeapot {
		t.Fatalf("recorded requests = %+v, want one GET of %s with status 418", requests, server.URL)
	}
}

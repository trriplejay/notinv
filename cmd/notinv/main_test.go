package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/joho/godotenv"

	"github.com/trriplejay/notinv/internal/config"
)

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

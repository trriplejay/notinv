package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// unsetConfigEnv isolates tests from the host and restores even originally unset
// variables via t.Setenv's cleanup. Tests modifying the environment are serial.
func unsetConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"NOTINV_ENV",
		"NOTINV_LISTEN",
		"DATABASE_URL",
		"NOTINV_RETENTION_DAYS",
		"NOTINV_LOG_LEVEL",
		"NOTINV_LOG_FORMAT",
		"NOTINV_DISCORD_TOKEN",
		"NOTINV_DISCORD_USER_ID",
	} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/notinv")
}

func assertConfig(t *testing.T, got, want Config) {
	t.Helper()
	for _, tt := range []struct {
		name string
		got  any
		want any
	}{
		{"Env", got.Env, want.Env},
		{"Listen", got.Listen, want.Listen},
		{"DatabaseURL", got.DatabaseURL, want.DatabaseURL},
		{"RetentionDays", got.RetentionDays, want.RetentionDays},
		{"LogLevel", got.LogLevel, want.LogLevel},
		{"LogFormat", got.LogFormat, want.LogFormat},
		{"DiscordToken", got.DiscordToken, want.DiscordToken},
		{"DiscordUserID", got.DiscordUserID, want.DiscordUserID},
		{"DiscordDryRun", got.DiscordDryRun, want.DiscordDryRun},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("field did not match expected value")
			}
		})
	}
}

// CLM-10: every documented default applies when all nine variables are unset.
func TestLoadDefaults(t *testing.T) {
	unsetConfigEnv(t)
	t.Setenv("DATABASE_URL", "")
	if err := os.Unsetenv("DATABASE_URL"); err != nil {
		t.Fatalf("unset DATABASE_URL: %v", err)
	}
	got, err := Load()
	if err == nil || !strings.Contains(err.Error(), "Postgres DATABASE_URL is required") {
		t.Fatalf("Load with unset DATABASE_URL error = %v, want error containing %q", err, "Postgres DATABASE_URL is required")
	}
	if got != nil {
		t.Fatalf("Load with unset DATABASE_URL returned non-nil config: %+v", got)
	}

	const postgresURL = "postgres://u:p@localhost:5432/notinv"
	t.Setenv("DATABASE_URL", postgresURL)
	got, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertConfig(t, *got, Config{
		Env:           "dev",
		Listen:        ":8080",
		DatabaseURL:   postgresURL,
		RetentionDays: 30,
		LogLevel:      "info",
		LogFormat:     "text",
		DiscordDryRun: true,
	})
}

// CLM-1: each of the nine explicit values overrides its default.
func TestLoadOverrides(t *testing.T) {
	unsetConfigEnv(t)
	for _, tt := range []struct {
		key   string
		value string
	}{
		{"NOTINV_ENV", "prod"},
		{"NOTINV_LISTEN", "127.0.0.1:9090"},
		{"DATABASE_URL", "postgres://example.test/db"},
		{"NOTINV_RETENTION_DAYS", "7"},
		{"NOTINV_LOG_LEVEL", "debug"},
		{"NOTINV_LOG_FORMAT", "json"},
		{"NOTINV_DISCORD_TOKEN", "discord-test-value"},
		{"NOTINV_DISCORD_USER_ID", "123456789"},
	} {
		t.Setenv(tt.key, tt.value)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertConfig(t, *got, Config{
		Env:           "prod",
		Listen:        "127.0.0.1:9090",
		DatabaseURL:   "postgres://example.test/db",
		RetentionDays: 7,
		LogLevel:      "debug",
		LogFormat:     "json",
		DiscordToken:  "discord-test-value",
		DiscordUserID: "123456789",
		DiscordDryRun: false,
	})
}

func TestLoadExplicitEmptyListen(t *testing.T) {
	unsetConfigEnv(t)
	t.Setenv("NOTINV_LISTEN", "")
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Listen != "" {
		t.Fatal("explicit empty listen was replaced by default")
	}
}

func checkEnvValue(t *testing.T, key, value string, wantErr bool) {
	t.Helper()
	unsetConfigEnv(t)
	t.Setenv(key, value)
	got, err := Load()
	if (err != nil) != wantErr {
		t.Fatalf("Load error = %v, want error %t", err, wantErr)
	}
	if !wantErr && got == nil {
		t.Fatal("Load returned nil config without error")
	}
}

// CLM-2: retention must parse as a positive integer.
func TestLoadRetention(t *testing.T) {
	for _, tt := range []struct {
		value   string
		wantErr bool
	}{
		{"0", true},
		{"-1", true},
		{"abc", true},
		{"", true},
		{"1.5", true},
		{"999999999999999999999999999999", true},
		{"1", false},
		{"365", false},
	} {
		t.Run(tt.value, func(t *testing.T) {
			checkEnvValue(t, "NOTINV_RETENTION_DAYS", tt.value, tt.wantErr)
		})
	}
}

// CLM-3: only the four documented log levels are accepted.
func TestLoadLogLevel(t *testing.T) {
	for _, tt := range []struct {
		value   string
		wantErr bool
	}{
		{"debug", false},
		{"info", false},
		{"warn", false},
		{"error", false},
		{"trace", true},
		{"INFO", true},
		{"", true},
	} {
		t.Run(tt.value, func(t *testing.T) {
			checkEnvValue(t, "NOTINV_LOG_LEVEL", tt.value, tt.wantErr)
		})
	}
}

func TestLoadLogFormat(t *testing.T) {
	for _, tt := range []struct {
		value   string
		wantErr bool
	}{
		{"text", false},
		{"json", false},
		{"yaml", true},
		{"JSON", true},
		{"", true},
	} {
		t.Run(tt.value, func(t *testing.T) {
			checkEnvValue(t, "NOTINV_LOG_FORMAT", tt.value, tt.wantErr)
		})
	}
}

func TestLoadEnv(t *testing.T) {
	for _, tt := range []struct {
		value   string
		wantErr bool
	}{
		{"dev", false},
		{"prod", false},
		{"staging", true},
		{"PROD", true},
		{"", true},
	} {
		t.Run(tt.value, func(t *testing.T) {
			checkEnvValue(t, "NOTINV_ENV", tt.value, tt.wantErr)
		})
	}
}

// Reject unsupported or malformed URLs.
func TestLoadDatabaseURL(t *testing.T) {
	for _, tt := range []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"empty", "", true},
		{"file", "file:./x.db", true},
		{"libsql", "libsql://example.test/db", true},
		{"https", "https://example.test/db", true},
		{"missing scheme", "./x.db", true},
		{"malformed", "postgres://user:secret@example.test/%zz", true},
		{"postgres", "postgres://u:p@h:5432/db", false},
		{"postgresql", "postgresql://u:p@h/db", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			unsetConfigEnv(t)
			t.Setenv("DATABASE_URL", tt.url)
			got, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if got != nil {
					t.Fatalf("expected nil config on error, got %+v", got)
				}
				if !strings.Contains(err.Error(), "Postgres DATABASE_URL is required") {
					t.Fatalf("error %q does not contain %q", err.Error(), "Postgres DATABASE_URL is required")
				}
				if tt.url != "" && strings.Contains(err.Error(), tt.url) {
					t.Fatalf("error %q echoed the URL", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("Load error = %v, want nil", err)
			}
			if got == nil || got.DatabaseURL != tt.url {
				t.Fatalf("Load did not preserve database URL: got %+v", got)
			}
		})
	}
}

// CLM-5: missing or empty Discord credentials enable dry-run.
func TestLoadDiscordDryRun(t *testing.T) {
	for _, explicitlyEmpty := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicitlyEmpty=%t", explicitlyEmpty), func(t *testing.T) {
			unsetConfigEnv(t)
			if explicitlyEmpty {
				t.Setenv("NOTINV_DISCORD_TOKEN", "")
				t.Setenv("NOTINV_DISCORD_USER_ID", "")
			}
			got, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !got.DiscordDryRun {
				t.Fatal("missing credentials must enable dry-run")
			}
		})
	}
}

// CLM-6: either partial Discord configuration fails; both values enable delivery.
func TestLoadDiscordCredentials(t *testing.T) {
	for _, tt := range []struct {
		name    string
		token   string
		userID  string
		wantErr bool
	}{
		{"token only", "test-token", "", true},
		{"user only", "", "123456789", true},
		{"both", "test-token", "123456789", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			unsetConfigEnv(t)
			if tt.token != "" {
				t.Setenv("NOTINV_DISCORD_TOKEN", tt.token)
			}
			if tt.userID != "" {
				t.Setenv("NOTINV_DISCORD_USER_ID", tt.userID)
			}
			got, err := Load()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load error = %v, want error %t", err, tt.wantErr)
			}
			if !tt.wantErr && (got == nil || got.DiscordDryRun) {
				t.Fatal("complete credentials must disable dry-run")
			}
		})
	}
}

func TestConfigLogValueFields(t *testing.T) {
	for _, databaseURL := range []string{"postgres://example.test/db", "postgresql://example.test:5432/notinv"} {
		t.Run(databaseURL, func(t *testing.T) {
			c := Config{
				Env:           "prod",
				Listen:        ":9090",
				DatabaseURL:   databaseURL,
				RetentionDays: 42,
				LogLevel:      "warn",
				LogFormat:     "json",
				DiscordUserID: "123456789",
				DiscordDryRun: true,
			}
			value := c.LogValue()
			if value.Kind() != slog.KindGroup {
				t.Fatal("LogValue must return a group")
			}
			got := make(map[string]any)
			for _, attr := range value.Group() {
				got[attr.Key] = attr.Value.Any()
			}
			expected := map[string]any{
				"env":             "prod",
				"listen":          ":9090",
				"database_url":    databaseURL,
				"retention_days":  int64(42),
				"log_level":       "warn",
				"log_format":      "json",
				"discord_token":   "[REDACTED]",
				"discord_user_id": "123456789",
				"discord_dry_run": true,
			}
			if len(got) != len(expected) {
				t.Fatalf("LogValue returned %d attributes, want %d: got keys %v", len(got), len(expected), got)
			}
			for key, want := range expected {
				if gotVal, ok := got[key]; !ok {
					t.Errorf("missing log field %s", key)
				} else if gotVal != want {
					t.Errorf("log field %s = %v, want %v", key, gotVal, want)
				}
			}
		})
	}
}

// CLM-8 / CLM-7: exercise String, standard formatting, and actual slog handlers with
// userinfo in password, username-only, escaped, opaque, and malformed URLs,
// as well as password-bearing query parameters (any case).
func TestConfigRedaction(t *testing.T) {
	const discordSecret = "SENTINEL_DISCORD_SECRET"
	const userSecret = "SENTINEL_URL_USER"         //nolint:gosec // G101 false positive: test sentinel, not a real credential
	const passwordSecret = "SENTINEL_URL_PASSWORD" //nolint:gosec // G101 false positive: test sentinel, not a real credential
	const sentinelPw = "S3cr3tPw"                  //nolint:gosec // G101 false positive: test sentinel, not a real credential
	for _, tt := range []struct {
		name string
		url  string
	}{
		{"password", "libsql://" + userSecret + ":" + passwordSecret + "@example.test/db"},
		{"username only", "libsql://" + userSecret + "@example.test/db"},
		{"escaped", "libsql://%53ENTINEL_URL_USER:%53ENTINEL_URL_PASSWORD@example.test/db"},
		{"malformed", "libsql://" + userSecret + ":" + passwordSecret + "@example.test/%zz"},
		{"opaque", "libsql:" + userSecret + ":" + passwordSecret + "@example.test/db"},
		{"userinfo with sentinel", "postgres://app:" + sentinelPw + "@example.test/db"},
		{"query param password lowercase", "postgres://example.test/db?sslmode=disable&password=" + sentinelPw},
		{"query param password uppercase", "postgres://example.test/db?PASSWORD=" + sentinelPw},
		{"query param password mixed case", "postgres://example.test/db?db_PassWord_val=" + sentinelPw},
		{"both userinfo and query param password", "postgres://app:" + sentinelPw + "@example.test/db?secret_password=" + sentinelPw},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{
				DatabaseURL:  tt.url,
				DiscordToken: discordSecret,
			}
			var jsonLog, textLog bytes.Buffer
			slog.New(slog.NewJSONHandler(&jsonLog, nil)).Info("config", "config", c)
			slog.New(slog.NewTextHandler(&textLog, nil)).Info("config", "config", &c)
			for _, form := range []struct {
				name   string
				output string
			}{
				{"String", c.String()},
				{"LogValue", c.LogValue().String()},
				{"fmt value", fmt.Sprintf("%v", c)},
				{"fmt detail", fmt.Sprintf("%+v", &c)},
				{"fmt Go", fmt.Sprintf("%#v", c)},
				{"slog JSON", jsonLog.String()},
				{"slog text", textLog.String()},
			} {
				t.Run(form.name, func(t *testing.T) {
					for _, secret := range []string{
						discordSecret, userSecret, passwordSecret,
						"%53ENTINEL_URL_USER", "%53ENTINEL_URL_PASSWORD",
						sentinelPw,
					} {
						if strings.Contains(form.output, secret) {
							t.Errorf("secret %q leaked in config representation: %s", secret, form.output)
						}
					}
				})
			}
			if c.DatabaseURL != tt.url || c.DiscordToken != discordSecret {
				t.Fatal("rendering mutated credentials")
			}
		})
	}
}

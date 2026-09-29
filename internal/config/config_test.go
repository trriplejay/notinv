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
		"NOTINV_LISTEN",
		"DATABASE_URL",
		"NOTINV_RETENTION_DAYS",
		"NOTINV_LOG_LEVEL",
		"NOTINV_LOG_FORMAT",
		"DATABASE_AUTH_TOKEN",
		"NOTINV_DISCORD_TOKEN",
		"NOTINV_DISCORD_USER_ID",
	} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
}

func assertConfig(t *testing.T, got, want Config) {
	t.Helper()
	for _, tt := range []struct {
		name string
		got  any
		want any
	}{
		{"Listen", got.Listen, want.Listen},
		{"DatabaseURL", got.DatabaseURL, want.DatabaseURL},
		{"RetentionDays", got.RetentionDays, want.RetentionDays},
		{"LogLevel", got.LogLevel, want.LogLevel},
		{"LogFormat", got.LogFormat, want.LogFormat},
		{"DatabaseAuthToken", got.DatabaseAuthToken, want.DatabaseAuthToken},
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

// CLM-10: every documented default applies when all eight variables are unset.
func TestLoadDefaults(t *testing.T) {
	unsetConfigEnv(t)
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertConfig(t, *got, Config{
		Listen:        ":8080",
		DatabaseURL:   "file:./data/notinv.db",
		RetentionDays: 30,
		LogLevel:      "info",
		LogFormat:     "text",
		DiscordDryRun: true,
	})
}

// CLM-1: each of the eight explicit values overrides its default.
func TestLoadOverrides(t *testing.T) {
	unsetConfigEnv(t)
	for _, tt := range []struct {
		key   string
		value string
	}{
		{"NOTINV_LISTEN", "127.0.0.1:9090"},
		{"DATABASE_URL", "libsql://example.test/db"},
		{"NOTINV_RETENTION_DAYS", "7"},
		{"NOTINV_LOG_LEVEL", "debug"},
		{"NOTINV_LOG_FORMAT", "json"},
		{"DATABASE_AUTH_TOKEN", "db-test-value"},
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
		Listen:            "127.0.0.1:9090",
		DatabaseURL:       "libsql://example.test/db",
		RetentionDays:     7,
		LogLevel:          "debug",
		LogFormat:         "json",
		DatabaseAuthToken: "db-test-value",
		DiscordToken:      "discord-test-value",
		DiscordUserID:     "123456789",
		DiscordDryRun:     false,
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

// CLM-4 and libsql authentication: reject unsupported or malformed URLs and
// require a separate auth token for libsql, even if the URL has userinfo.
func TestLoadDatabaseURL(t *testing.T) {
	for _, tt := range []struct {
		name    string
		url     string
		token   string
		wantErr bool
	}{
		{"file", "file:./x.db", "", false},
		{"libsql", "libsql://example.test/db", "test-auth", false},
		{"postgres", "postgres://example.test/db", "test-auth", true},
		{"https", "https://example.test/db", "test-auth", true},
		{"missing scheme", "./x.db", "", true},
		{"empty", "", "", true},
		{"malformed", "libsql://user:secret@example.test/%zz", "test-auth", true},
		{"libsql no auth", "libsql://example.test/db", "", true},
		{"userinfo is not auth", "libsql://user:secret@example.test/db", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			unsetConfigEnv(t)
			t.Setenv("DATABASE_URL", tt.url)
			t.Setenv("DATABASE_AUTH_TOKEN", tt.token)
			got, err := Load()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load error = %v, want error %t", err, tt.wantErr)
			}
			if !tt.wantErr && (got == nil || got.DatabaseURL != tt.url) {
				t.Fatal("Load did not preserve database URL")
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
	for _, databaseURL := range []string{"file:./data/notinv.db", "libsql://example.test/db"} {
		t.Run(databaseURL, func(t *testing.T) {
			c := Config{
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
			for key, want := range map[string]any{
				"listen":              ":9090",
				"database_url":        databaseURL,
				"retention_days":      int64(42),
				"log_level":           "warn",
				"log_format":          "json",
				"database_auth_token": "[REDACTED]",
				"discord_token":       "[REDACTED]",
				"discord_user_id":     "123456789",
				"discord_dry_run":     true,
			} {
				if got[key] != want {
					t.Errorf("unexpected log field %s", key)
				}
			}
		})
	}
}

// CLM-8: exercise String, standard formatting, and actual slog handlers with
// userinfo in password, username-only, escaped, opaque, and malformed URLs.
func TestConfigRedaction(t *testing.T) {
	const discordSecret = "SENTINEL_DISCORD_SECRET"
	const authSecret = "SENTINEL_DB_AUTH_SECRET"   //nolint:gosec // G101 false positive: test sentinel, not a real credential
	const userSecret = "SENTINEL_URL_USER"         //nolint:gosec // G101 false positive: test sentinel, not a real credential
	const passwordSecret = "SENTINEL_URL_PASSWORD" //nolint:gosec // G101 false positive: test sentinel, not a real credential
	for _, tt := range []struct {
		name string
		url  string
	}{
		{"password", "libsql://" + userSecret + ":" + passwordSecret + "@example.test/db"},
		{"username only", "libsql://" + userSecret + "@example.test/db"},
		{"escaped", "libsql://%53ENTINEL_URL_USER:%53ENTINEL_URL_PASSWORD@example.test/db"},
		{"malformed", "libsql://" + userSecret + ":" + passwordSecret + "@example.test/%zz"},
		{"opaque", "libsql:" + userSecret + ":" + passwordSecret + "@example.test/db"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{
				DatabaseURL:       tt.url,
				DatabaseAuthToken: authSecret,
				DiscordToken:      discordSecret,
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
					if !strings.Contains(form.output, "[REDACTED]") {
						t.Error("missing redaction marker")
					}
					for _, secret := range []string{
						discordSecret, authSecret, userSecret, passwordSecret,
						"%53ENTINEL_URL_USER", "%53ENTINEL_URL_PASSWORD",
					} {
						if strings.Contains(form.output, secret) {
							t.Error("secret leaked in config representation")
						}
					}
				})
			}
			if c.DatabaseURL != tt.url || c.DatabaseAuthToken != authSecret || c.DiscordToken != discordSecret {
				t.Fatal("rendering mutated credentials")
			}
		})
	}
}

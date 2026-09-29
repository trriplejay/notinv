package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config holds the validated startup settings for notinv.
// String and LogValue redact credentials; the fields retain their original values.
type Config struct {
	Listen            string
	DatabaseURL       string
	RetentionDays     int
	LogLevel          string
	LogFormat         string
	DatabaseAuthToken string
	DiscordToken      string
	DiscordUserID     string
	DiscordDryRun     bool
}

// Load reads the environment and validates startup settings.
// Defaults apply only to unset variables. Empty Discord credentials disable
// notifications; supplying only one of the two credentials is an error.
func Load() (*Config, error) {
	c := Config{
		Listen:            envOrDefault("NOTINV_LISTEN", ":8080"),
		DatabaseURL:       envOrDefault("DATABASE_URL", "file:./data/notinv.db"),
		LogLevel:          envOrDefault("NOTINV_LOG_LEVEL", "info"),
		LogFormat:         envOrDefault("NOTINV_LOG_FORMAT", "text"),
		DatabaseAuthToken: os.Getenv("DATABASE_AUTH_TOKEN"),
		DiscordToken:      os.Getenv("NOTINV_DISCORD_TOKEN"),
		DiscordUserID:     os.Getenv("NOTINV_DISCORD_USER_ID"),
	}

	retention, err := strconv.Atoi(envOrDefault("NOTINV_RETENTION_DAYS", "30"))
	if err != nil {
		return nil, fmt.Errorf("NOTINV_RETENTION_DAYS must be a positive integer: %w", err)
	}
	if retention <= 0 {
		return nil, errors.New("NOTINV_RETENTION_DAYS must be a positive integer")
	}
	c.RetentionDays = retention

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return nil, errors.New("NOTINV_LOG_LEVEL must be debug, info, warn, or error")
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return nil, errors.New("NOTINV_LOG_FORMAT must be text or json")
	}

	databaseURL, err := url.Parse(c.DatabaseURL)
	if err != nil {
		// url.Parse errors include the input URL, which may contain credentials.
		return nil, errors.New("DATABASE_URL must be a valid URL")
	}
	switch databaseURL.Scheme {
	case "file":
	case "libsql":
		if c.DatabaseAuthToken == "" {
			return nil, errors.New("DATABASE_AUTH_TOKEN is required for a libsql DATABASE_URL")
		}
	default:
		return nil, errors.New("DATABASE_URL scheme must be file or libsql")
	}

	if (c.DiscordToken == "") != (c.DiscordUserID == "") {
		return nil, errors.New("NOTINV_DISCORD_TOKEN and NOTINV_DISCORD_USER_ID must both be provided or both be empty")
	}
	c.DiscordDryRun = c.DiscordToken == ""
	return &c, nil
}

func envOrDefault(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// String returns a diagnostic representation with credentials redacted.
func (c Config) String() string {
	return c.LogValue().String()
}

// GoString also redacts credentials when Config is formatted with %#v.
func (c Config) GoString() string {
	return c.String()
}

// LogValue returns structured startup settings without exposing credentials.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("listen", c.Listen),
		slog.String("database_url", redactedDatabaseURL(c.DatabaseURL)),
		slog.Int("retention_days", c.RetentionDays),
		slog.String("log_level", c.LogLevel),
		slog.String("log_format", c.LogFormat),
		slog.String("database_auth_token", "[REDACTED]"),
		slog.String("discord_token", "[REDACTED]"),
		slog.String("discord_user_id", c.DiscordUserID),
		slog.Bool("discord_dry_run", c.DiscordDryRun),
	)
}

func redactedDatabaseURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "[REDACTED]"
	}
	// Opaque URLs do not populate User; avoid printing credential-like content.
	if strings.Contains(parsed.Opaque, "@") {
		return "[REDACTED]"
	}
	parsed.User = nil
	return parsed.String()
}

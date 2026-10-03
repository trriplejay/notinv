package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trriplejay/notinv/internal/config"
)

// Sender is the notification contract, also implementable by test fakes.
type Sender interface {
	Send(ctx context.Context, msg string) error
}

// Options overrides the Discord endpoint and delivery dependencies.
// Zero values select the defaults. Sleep implementations must be concurrency-safe.
type Options struct {
	BaseURL     string
	HTTPClient  *http.Client
	Sleep       func(time.Duration)
	MaxAttempts int
}

// Notifier delivers direct messages to one configured Discord recipient.
// It is safe for concurrent use and must not be copied after first use.
type Notifier struct {
	token, recipient, baseURL string
	dryRun                    bool
	log                       *slog.Logger
	client                    *http.Client
	sleep                     func(time.Duration)
	maxAttempts               int
	mu                        sync.Mutex
	channelID                 string
}

var _ Sender = (*Notifier)(nil)

// New constructs a notifier from the Discord settings in cfg.
// Defaults are Discord API v10, a 30-second HTTP timeout, time.Sleep, and
// four attempts per API request. A missing token always enables dry-run.
func New(cfg config.Config, log *slog.Logger, opts Options) *Notifier {
	if opts.BaseURL == "" {
		opts.BaseURL = "https://discord.com/api/v10"
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 4
	}
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{
		token: cfg.DiscordToken, recipient: cfg.DiscordUserID,
		dryRun:  cfg.DiscordDryRun || cfg.DiscordToken == "",
		baseURL: strings.TrimRight(opts.BaseURL, "/"), log: log,
		client: opts.HTTPClient, sleep: opts.Sleep, maxAttempts: opts.MaxAttempts,
	}
}

// Send logs a dry-run message or delivers it to the configured Discord user.
// Transient failures are retried; ambiguous network failures can cause duplicate
// deliveries because Discord may have accepted a request before it failed locally.
func (n *Notifier) Send(ctx context.Context, msg string) error {
	if n.dryRun {
		n.log.InfoContext(ctx, "notify (dry-run)", slog.String("message", msg))
		return nil
	}
	if n.recipient == "" {
		return errors.New("discord recipient is required")
	}
	id, err := n.resolveChannel(ctx)
	if err != nil {
		return fmt.Errorf("resolve Discord DM channel: %w", err)
	}
	_, err = n.post(ctx, "/channels/"+url.PathEscape(id)+"/messages", map[string]string{"content": msg})
	if err != nil {
		return fmt.Errorf("send Discord message: %w", err)
	}
	return nil
}

func (n *Notifier) resolveChannel(ctx context.Context) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.channelID != "" {
		return n.channelID, nil
	}
	body, err := n.post(ctx, "/users/@me/channels", map[string]string{"recipient_id": n.recipient})
	if err != nil {
		return "", err
	}
	var channel struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &channel); err != nil {
		return "", fmt.Errorf("decode channel: %w", err)
	}
	if channel.ID == "" {
		return "", errors.New("discord returned an empty channel id")
	}
	n.channelID = channel.ID
	return n.channelID, nil
}

func (n *Notifier) post(ctx context.Context, path string, payload map[string]string) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode Discord request: %w", err)
	}
	for attempt := 0; attempt < n.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, retry, delay, err := n.attempt(ctx, path, body)
		if err == nil {
			return data, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retry || attempt == n.maxAttempts-1 {
			return nil, err
		}
		if delay < 0 {
			delay = time.Second * time.Duration(1<<min(attempt, 3))
		}
		n.sleep(delay)
	}
	return nil, errors.New("discord retry attempts exhausted")
}

func (n *Notifier) attempt(ctx context.Context, path string, body []byte) ([]byte, bool, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, false, 0, fmt.Errorf("create Discord request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+n.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		// net/http closes any response body returned alongside an error.
		return nil, true, -1, fmt.Errorf("discord request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, true, -1, fmt.Errorf("read Discord response: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return data, false, 0, nil
	}
	retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
	delay := time.Duration(-1)
	if resp.StatusCode == http.StatusTooManyRequests {
		delay = retryDelay(data, resp.Header.Get("Retry-After"))
	}
	return nil, retry, delay, fmt.Errorf("discord returned HTTP %d", resp.StatusCode)
}

func retryDelay(body []byte, header string) time.Duration {
	var rateLimit struct {
		RetryAfter *float64 `json:"retry_after"`
	}
	seconds, err := strconv.ParseFloat(header, 64)
	if json.Unmarshal(body, &rateLimit) == nil && rateLimit.RetryAfter != nil {
		seconds, err = *rateLimit.RetryAfter, nil
	}
	// Reject negative, non-finite, and overflowing durations.
	if err == nil && seconds >= 0 && seconds < float64(1<<63-1)/float64(time.Second) {
		return time.Duration(seconds * float64(time.Second))
	}
	return -1
}

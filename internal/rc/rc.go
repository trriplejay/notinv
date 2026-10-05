// Package rc provides HTTP clients that asynchronously record request results.
package rc

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/trriplejay/notinv/internal/store"
)

// Options configures the writer. Non-positive values use the defaults.
type Options struct {
	BufferSize    int
	BatchSize     int
	FlushInterval time.Duration
}

// Writer persists buffered request records from a single background goroutine.
// Close it before closing the store. Records arriving after Close are dropped.
type Writer struct {
	store     *store.Store
	log       *slog.Logger
	userAgent string
	records   chan store.Request
	batchSize int
	interval  time.Duration
	stop      chan struct{}
	finished  chan struct{}

	// The mutex orders enqueue against shutdown; it is never held during I/O.
	mu     sync.Mutex
	closed bool
}

// NewWriter starts a writer using s and the supplied application version.
// A nil logger uses slog.Default. The store must be non-nil and remain open
// until Close returns. Defaults are 1024 buffered records, batches of 100,
// and a five-second flush interval.
func NewWriter(s *store.Store, version string, log *slog.Logger, options Options) *Writer {
	if log == nil {
		log = slog.Default()
	}
	if version == "" {
		version = "dev"
	}
	if options.BufferSize <= 0 {
		options.BufferSize = 1024
	}
	if options.BatchSize <= 0 {
		options.BatchSize = 100
	}
	if options.FlushInterval <= 0 {
		options.FlushInterval = 5 * time.Second
	}
	w := &Writer{
		store: s, log: log, userAgent: "notinv/" + version,
		records:   make(chan store.Request, options.BufferSize),
		batchSize: options.BatchSize, interval: options.FlushInterval,
		stop: make(chan struct{}), finished: make(chan struct{}),
	}
	go w.run()
	return w
}

// New returns a client for the named script, wrapping http.DefaultTransport
// with recording through writer. The writer must be non-nil. The client has
// a 30-second timeout, which callers may override for their script.
func New(name string, writer *Writer) *http.Client {
	return &http.Client{
		Transport: &transport{name: name, writer: writer, base: http.DefaultTransport},
		Timeout:   30 * time.Second,
	}
}

type transport struct {
	name   string
	writer *Writer
	base   http.RoundTripper
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	outbound := req.Clone(req.Context())
	if outbound.Header == nil {
		outbound.Header = make(http.Header)
	}
	if outbound.Header.Get("User-Agent") == "" {
		outbound.Header.Set("User-Agent", t.writer.userAgent)
	}
	start := time.Now()
	resp, err := t.base.RoundTrip(outbound)
	record := store.Request{
		Script: t.name, Method: req.Method, URL: req.URL.String(),
		StartedAt: start, DurationMs: time.Since(start).Milliseconds(),
	}
	if err != nil {
		message := err.Error()
		record.Err = &message
	} else if resp != nil {
		record.StatusCode = resp.StatusCode
	}
	// Recording is a side effect only: never replace the delegate's result.
	t.writer.enqueue(record)
	return resp, err
}

func (w *Writer) enqueue(record store.Request) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		w.log.Warn("dropping request record, writer closed")
		return
	}
	select {
	case w.records <- record:
		w.mu.Unlock()
	default:
		w.mu.Unlock()
		w.log.Warn("dropping request record, buffer full")
	}
}

// Close stops accepting records, drains the buffer, and waits for persistence
// to finish. It is safe to call concurrently or repeatedly. Insert failures are
// logged and their batches discarded; Close does not retry failed batches.
func (w *Writer) Close() {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.stop)
	}
	w.mu.Unlock()
	<-w.finished
}

func (w *Writer) run() {
	defer close(w.finished)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	batch := make([]store.Request, 0, w.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		// A signal-cancelled application context must not cancel the final flush.
		if err := w.store.InsertRequests(context.Background(), batch); err != nil {
			w.log.Warn("failed to persist request records", "err", err, "count", len(batch))
		}
		batch = batch[:0]
	}
	appendRecord := func(record store.Request) {
		batch = append(batch, record)
		if len(batch) >= w.batchSize {
			flush()
		}
	}
	for {
		select {
		case record := <-w.records:
			appendRecord(record)
		case <-ticker.C:
			flush()
		case <-w.stop:
			// Close excludes new sends before signalling stop. The channel is
			// deliberately never closed, so concurrent requests cannot panic.
			for {
				select {
				case record := <-w.records:
					appendRecord(record)
				default:
					flush()
					return
				}
			}
		}
	}
}

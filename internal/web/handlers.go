package web

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/trriplejay/notinv/internal/store"
)

// RunQuerier reads a script's runs in ascending start-time order.
type RunQuerier interface {
	QueryRuns(ctx context.Context, script string, since, until time.Time) ([]store.Run, error)
}

// ScriptLister supplies script names, latest runs, and windowed run history.
type ScriptLister interface {
	RunQuerier
	ListScripts(ctx context.Context) ([]string, error)
	LatestRun(ctx context.Context, script string) (*store.Run, error)
}

// RequestQuerier reads a script's requests within inclusive time bounds.
type RequestQuerier interface {
	QueryRequests(ctx context.Context, script string, since, until time.Time) ([]store.Request, error)
}

// Pinger checks database reachability.
type Pinger interface {
	Ping(ctx context.Context) error
}

type latestDTO struct {
	Time  string  `json:"time"`
	OK    bool    `json:"ok"`
	Error *string `json:"error"`
}

type scriptDTO struct {
	Name     string     `json:"name"`
	Latest   *latestDTO `json:"latest"`
	Uptime   float64    `json:"uptime"`
	Schedule string     `json:"schedule"`
}

type runDTO struct {
	Start    string  `json:"start"`
	OK       bool    `json:"ok"`
	Duration int64   `json:"duration"`
	Error    *string `json:"error"`
}

type requestDTO struct {
	URL        string  `json:"url"`
	Count      int     `json:"count"`
	Errors     int     `json:"errors"`
	AvgLatency float64 `json:"avgLatency"`
}

// NewScriptsHandler serves GET /api/scripts. Uptime is an OK/total ratio over
// the last seven days. A nil now uses time.Now.
func NewScriptsHandler(db ScriptLister, now func() time.Time) http.Handler {
	now = clock(now)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		until := now()
		since := until.Add(-7 * 24 * time.Hour)
		names, err := db.ListScripts(r.Context())
		if err != nil {
			http.Error(w, "cannot list scripts", http.StatusInternalServerError)
			return
		}
		out := make([]scriptDTO, 0, len(names))
		for _, name := range names {
			latest, err := db.LatestRun(r.Context(), name)
			if err != nil {
				http.Error(w, "cannot read latest run", http.StatusInternalServerError)
				return
			}
			runs, err := db.QueryRuns(r.Context(), name, since, until)
			if err != nil {
				http.Error(w, "cannot read runs", http.StatusInternalServerError)
				return
			}
			item := scriptDTO{Name: name, Schedule: ""}
			if latest != nil {
				item.Latest = &latestDTO{Time: latest.StartedAt.Format(time.RFC3339), OK: latest.OK, Error: latest.Err}
			}
			ok := 0
			for _, run := range runs {
				if run.OK {
					ok++
				}
			}
			if len(runs) > 0 {
				item.Uptime = float64(ok) / float64(len(runs))
			}
			out = append(out, item)
		}
		writeJSON(w, out)
	})
}

// NewRunsHandler serves GET /api/scripts/{name}/runs. A nil now uses time.Now.
func NewRunsHandler(db RunQuerier, now func() time.Time) http.Handler {
	now = clock(now)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		until := now()
		since, err := windowStart(r, until)
		if err != nil {
			http.Error(w, "since must be RFC3339", http.StatusBadRequest)
			return
		}
		runs, err := db.QueryRuns(r.Context(), r.PathValue("name"), since, until)
		if err != nil {
			http.Error(w, "cannot read runs", http.StatusInternalServerError)
			return
		}
		out := make([]runDTO, 0, len(runs))
		for _, run := range runs {
			out = append(out, runDTO{
				Start: run.StartedAt.Format(time.RFC3339), OK: run.OK,
				Duration: run.DurationMs, Error: run.Err,
			})
		}
		writeJSON(w, out)
	})
}

// NewRequestsHandler serves GET /api/scripts/{name}/requests with one aggregate
// per URL, in first-seen order. A nil now uses time.Now.
func NewRequestsHandler(db RequestQuerier, now func() time.Time) http.Handler {
	now = clock(now)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		until := now()
		since, err := windowStart(r, until)
		if err != nil {
			http.Error(w, "since must be RFC3339", http.StatusBadRequest)
			return
		}
		requests, err := db.QueryRequests(r.Context(), r.PathValue("name"), since, until)
		if err != nil {
			http.Error(w, "cannot read requests", http.StatusInternalServerError)
			return
		}
		out := []requestDTO{}
		indices := make(map[string]int)
		for _, request := range requests {
			i, exists := indices[request.URL]
			if !exists {
				i = len(out)
				indices[request.URL] = i
				out = append(out, requestDTO{URL: request.URL})
			}
			item := &out[i]
			item.Count++
			if request.StatusCode >= 400 || (request.Err != nil && *request.Err != "") {
				item.Errors++
			}
			item.AvgLatency += float64(request.DurationMs)
		}
		for i := range out {
			if out[i].Count > 0 {
				out[i].AvgLatency /= float64(out[i].Count)
			}
		}
		writeJSON(w, out)
	})
}

// NewHealthHandler serves GET /healthz, returning 503 when the database ping fails.
func NewHealthHandler(db Pinger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func clock(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}

func windowStart(r *http.Request, until time.Time) (time.Time, error) {
	if since := r.URL.Query().Get("since"); since != "" {
		return time.Parse(time.RFC3339, since)
	}
	return until.Add(-7 * 24 * time.Hour), nil
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	// DTOs contain only JSON-safe values. A write failure cannot be repaired by
	// writing a second response after headers/body have already been sent.
	_ = json.NewEncoder(w).Encode(value)
}

// Package lunch sends each school morning's MealViewer lunch menu.
package lunch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	// Embed zoneinfo so America/Los_Angeles loads on images without tzdata.
	_ "time/tzdata"

	"github.com/trriplejay/notinv/internal/runner"
)

const (
	defaultSchool = "Bryant"
	pacificZone   = "America/Los_Angeles"
	// maxBody bounds how much of a menu response is decoded.
	maxBody = 4 << 20
)

// Script notifies the day's lunch menu at 06:00 Pacific time. Its zero value is
// ready to use. Reuse the same instance for successive runs; state is in memory
// only and Run calls on an instance must be serial, not concurrent.
type Script struct {
	// hasRun makes the runner's immediate startup run a silent no-op.
	hasRun bool
	// now supplies the current instant; nil means time.Now.
	now func() time.Time
}

var _ runner.Script = (*Script)(nil)

// New returns a ready-to-use *Script.
func New() *Script { return &Script{} }

// Name returns the stable identifier used for this script's logs and results.
func (*Script) Name() string { return "lunch" }

// Schedule fires at 06:00 Los Angeles local time, across daylight saving changes.
func (*Script) Schedule() string { return "CRON_TZ=" + pacificZone + " 0 6 * * *" }

// Run skips its first call (the runner's startup run); every later call fetches
// today's menu and sends it. A failed fetch is reported by notification and
// returned as an error; a notification failure is returned wrapped.
func (s *Script) Run(ctx context.Context, rc *runner.Context) error {
	if !s.hasRun {
		s.hasRun = true
		return nil
	}
	items, fetchErr := s.fetchLunch(ctx, rc)
	var msg string
	switch {
	case fetchErr != nil:
		msg = "The lunch menu could not be retrieved today."
	case len(items) == 0:
		msg = "No lunch menu was found for today."
	default:
		msg = "Today's lunch menu:\n" + strings.Join(items, "\n")
	}
	var notifyErr error
	if err := rc.Notify(ctx, msg); err != nil {
		notifyErr = fmt.Errorf("notify lunch menu: %w", err)
	}
	rc.Log.InfoContext(ctx, "lunch check completed", "items", len(items), "error", fetchErr)
	return errors.Join(fetchErr, notifyErr)
}

func (s *Script) fetchLunch(ctx context.Context, rc *runner.Context) ([]string, error) {
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	target, err := menuURL(school(), now())
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil) //nolint:gosec // G704 false positive: the URL targets the fixed MealViewer host; only the escaped school path segment is configurable
	if err != nil {
		return nil, fmt.Errorf("build lunch request: %w", err)
	}
	resp, err := rc.HTTP.Do(req) //nolint:gosec // G704 false positive: the request targets the fixed MealViewer host
	if err != nil {
		return nil, fmt.Errorf("perform lunch request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			rc.Log.WarnContext(ctx, "close lunch response body", "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lunch menu: unexpected HTTP status %d", resp.StatusCode)
	}
	var doc menuResponse
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, maxBody)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode lunch menu: %w", err)
	}
	return lunchItems(doc), nil
}

// school returns the configured school path segment, escaped for use in a URL.
func school() string {
	if v := os.Getenv("LUNCH_SCHOOL"); v != "" {
		return url.PathEscape(v)
	}
	return defaultSchool
}

// menuURL builds the one-day MealViewer request for the Pacific date of at.
// school must already be path-escaped.
func menuURL(school string, at time.Time) (string, error) {
	loc, err := time.LoadLocation(pacificZone)
	if err != nil {
		return "", fmt.Errorf("load %s: %w", pacificZone, err)
	}
	date := at.In(loc).Format("01-02-2006")
	return "https://api.mealviewer.com/api/v4/school/" + school + "/" + date + "/" + date + "/0", nil
}

// menuResponse mirrors the parts of the MealViewer v4 response that are used.
type menuResponse struct {
	MenuSchedules []struct {
		MenuBlocks []struct {
			BlockName         string `json:"blockName"`
			CafeteriaLineList struct {
				Data []struct {
					FoodItemList struct {
						Data []struct {
							ItemName string `json:"item_Name"`
						} `json:"data"`
					} `json:"foodItemList"`
				} `json:"data"`
			} `json:"cafeteriaLineList"`
		} `json:"menuBlocks"`
	} `json:"menuSchedules"`
}

// lunchItems returns the item names of every Lunch block; breakfast and other
// blocks are ignored. Ranging over empty or missing lists yields no items.
func lunchItems(doc menuResponse) []string {
	var items []string
	for _, schedule := range doc.MenuSchedules {
		for _, block := range schedule.MenuBlocks {
			if !strings.EqualFold(strings.TrimSpace(block.BlockName), "Lunch") {
				continue
			}
			for _, line := range block.CafeteriaLineList.Data {
				for _, food := range line.FoodItemList.Data {
					if name := strings.TrimSpace(food.ItemName); name != "" {
						items = append(items, name)
					}
				}
			}
		}
	}
	return items
}

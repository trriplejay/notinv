// Package nerdygurdy watches the Nerdy Gurdy NG Linotte kit during the shop's
// Sunday-evening restock window and notifies when it comes back into stock.
package nerdygurdy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	// Embed zoneinfo so Europe/Amsterdam loads on images without tzdata.
	_ "time/tzdata"

	"github.com/trriplejay/notinv/internal/runner"
)

const productURL = "https://www.nerdygurdy.nl/product/ng-linotte-kit/"

// The restock window: once a minute on Sundays 19:00-22:59 Amsterdam time,
// around the shop's posted 20:30 restock. The schedule and the bounds inWindow
// uses describe the same window and must change together.
const (
	amsterdamZone = "Europe/Amsterdam"
	schedule      = "CRON_TZ=Europe/Amsterdam * 19-22 * * 0"
	windowDay     = time.Sunday
	windowFirst   = 19 // first hour in the window
	windowLast    = 22 // last hour in the window
)

const (
	// maxBody bounds how much of the product page is read (the page is ~165 KB).
	maxBody = 2 << 20
	// restockSentence is the shop's posted schedule. The script does not read
	// its schedule from the page; a changed sentence is only logged so the
	// fixed window can be revisited.
	restockSentence = "New stock is added each Sunday around 20:30 Central European Time."
)

// loadLocation resolves the window's zone; tests replace it to simulate a
// missing zone database.
var loadLocation = time.LoadLocation

// Script checks the kit's stock and notifies on transitions into stock.
// Its zero value is ready to use. Reuse the same instance for successive runs;
// state is in memory only and resets when the process restarts. Run calls on an
// instance must be serial, not concurrent.
type Script struct {
	// started marks that the runner's immediate startup run has been handled.
	started bool
	// hasRun distinguishes the silent baseline from subsequent observations.
	hasRun bool
	// lastInStock records the last observation, independently of notification success.
	lastInStock bool
	// now supplies the current instant; nil means time.Now.
	now func() time.Time
}

var _ runner.Script = (*Script)(nil)

// New returns a ready-to-use *Script (its documented zero value).
func New() *Script { return &Script{} }

// Name returns the stable identifier used for this script's logs and results.
func (*Script) Name() string { return "nerdygurdy" }

// Schedule fires once a minute on Sundays 19:00-22:59 Amsterdam local time,
// across daylight saving changes.
func (*Script) Schedule() string { return schedule }

// Run checks the product and notifies only on a transition into stock. The
// runner's startup run outside the window is a silent no-op, so the script is
// dormant until the next window. Failed checks are not observations and leave
// the last known state intact.
func (s *Script) Run(ctx context.Context, rc *runner.Context) error {
	if !s.started {
		now := time.Now
		if s.now != nil {
			now = s.now
		}
		in, err := inWindow(now())
		if err != nil {
			return err
		}
		s.started = true
		if !in {
			rc.Log.InfoContext(ctx, "nerdygurdy startup outside window; dormant")
			return nil
		}
	}
	stocked, checkErr := s.check(ctx, rc)
	rc.Log.InfoContext(ctx, "nerdygurdy check completed", "in_stock", stocked, "error", checkErr)
	if checkErr != nil {
		return checkErr
	}
	changed := s.hasRun && !s.lastInStock && stocked
	// Record the observation before sending: a failed notification must not
	// cause retries on steady states or swallow a later genuine transition.
	s.hasRun, s.lastInStock = true, stocked
	if changed {
		if err := rc.Notify(ctx, "The NG Linotte kit is back in stock at Nerdy Gurdy: "+productURL); err != nil {
			return fmt.Errorf("notify nerdygurdy restock: %w", err)
		}
	}
	return nil
}

// inWindow reports whether t, in Amsterdam time, falls inside the restock window.
func inWindow(t time.Time) (bool, error) {
	loc, err := loadLocation(amsterdamZone)
	if err != nil {
		return false, fmt.Errorf("load %s: %w", amsterdamZone, err)
	}
	local := t.In(loc)
	return local.Weekday() == windowDay && local.Hour() >= windowFirst && local.Hour() <= windowLast, nil
}

func (*Script) check(ctx context.Context, rc *runner.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, productURL, nil) //nolint:gosec // G704 false positive: the fixed Nerdy Gurdy product URL is the required monitoring target, not user input
	if err != nil {
		return false, fmt.Errorf("build nerdygurdy request: %w", err)
	}
	// Use the injected client for its timeout and replaceable transport.
	resp, err := rc.HTTP.Do(req) //nolint:gosec // G704 false positive: fetching the fixed Nerdy Gurdy product URL is the required stock-check behavior, not an SSRF vector
	if err != nil {
		return false, fmt.Errorf("perform nerdygurdy request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			rc.Log.WarnContext(ctx, "close nerdygurdy response body", "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("nerdygurdy check: unexpected HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(http.MaxBytesReader(nil, resp.Body, maxBody))
	if err != nil {
		return false, fmt.Errorf("read nerdygurdy response: %w", err)
	}
	ok, err := inStock(body)
	if found, matches := restockLine(body); !matches {
		rc.Log.WarnContext(ctx, "nerdygurdy restock sentence changed; review the fixed window",
			"expected", restockSentence, "found", found)
	}
	return ok, err
}

var restockText = regexp.MustCompile(`(?i)new stock[^<]{0,200}`)

// restockLine reports whether the posted restock sentence is unchanged and,
// when it is not, what the page says instead ("<absent>" when nothing is found).
func restockLine(body []byte) (string, bool) {
	if strings.Contains(string(body), restockSentence) {
		return restockSentence, true
	}
	if m := restockText.Find(body); m != nil {
		return strings.TrimSpace(string(m)), false
	}
	return "<absent>", false
}

var (
	// productWrapper matches the opening tag of the kit's own product element,
	// in any attribute order and either quote style.
	productWrapper = regexp.MustCompile(`(?is)<[a-z]+\b[^>]*\sid\s*=\s*["']product-2113["'][^>]*>`)
	// classAttr captures a tag's class attribute value.
	classAttr = regexp.MustCompile(`(?is)\sclass\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	// stockPara matches paragraph opening tags; its class list decides whether it is p.stock.
	stockPara = regexp.MustCompile(`(?is)<p\b[^>]*>`)
	// otherProduct matches the opening tag of another product (related or upsell
	// items), which ends the kit's own region.
	otherProduct = regexp.MustCompile(`(?is)<[a-z]+\b[^>]*\sclass\s*=\s*["'][^"']*\btype-product\b[^>]*>`)
)

func classes(tag []byte) []string {
	m := classAttr.FindSubmatch(tag)
	if m == nil {
		return nil
	}
	return strings.Fields(string(m[1]) + " " + string(m[2]))
}

// stockToken returns the single known stock state among tokens. Neither token,
// both, or an unknown stock token is an error, never a guessed state.
func stockToken(tokens []string, in, out string, unknown ...string) (bool, error) {
	var hasIn, hasOut bool
	for _, tok := range tokens {
		switch tok {
		case in:
			hasIn = true
		case out:
			hasOut = true
		default:
			for _, u := range unknown {
				if tok == u {
					return false, fmt.Errorf("nerdygurdy check: unsupported stock status %q", tok)
				}
			}
		}
	}
	if hasIn == hasOut {
		return false, fmt.Errorf("nerdygurdy check: ambiguous stock classes %q", strings.Join(tokens, " "))
	}
	return hasIn, nil
}

// inStock decides the kit's stock from its product wrapper's class list
// (instock/outofstock) and, when present, the first p.stock inside the kit's
// own region (before any related or upsell product). The two must agree.
// Stock classes elsewhere on the page never count.
func inStock(body []byte) (bool, error) {
	loc := productWrapper.FindIndex(body)
	if loc == nil {
		return false, errors.New("nerdygurdy: stock status not found")
	}
	state, err := stockToken(classes(body[loc[0]:loc[1]]), "instock", "outofstock", "onbackorder")
	if err != nil {
		return false, err
	}
	region := body[loc[1]:]
	if next := otherProduct.FindIndex(region); next != nil {
		region = region[:next[0]]
	}
	for _, p := range stockPara.FindAll(region, -1) {
		tokens := classes(p)
		isStock := false
		for _, tok := range tokens {
			isStock = isStock || tok == "stock"
		}
		if !isStock {
			continue
		}
		para, err := stockToken(tokens, "in-stock", "out-of-stock", "available-on-backorder")
		if err != nil {
			return false, err
		}
		if para != state {
			return false, fmt.Errorf("nerdygurdy check: product classes (in stock %t) disagree with stock paragraph (in stock %t)", state, para)
		}
		return state, nil
	}
	return state, nil
}

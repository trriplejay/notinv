// Package nintendo monitors the Electric OCARINA OF TIME storefront offer.
package nintendo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"github.com/trriplejay/notinv/internal/runner"
)

const productURL = "https://www.nintendo.com/us/store/products/electric-ocarina-of-time-129987/"

// Script checks purchasability and notifies on transitions into stock.
// Its zero value is ready to use. Reuse the same instance for successive runs;
// state is in memory only and resets when the process restarts. Run calls on an
// instance must be serial, not concurrent.
type Script struct {
	// hasRun distinguishes the silent baseline from subsequent observations.
	hasRun bool
	// lastOK records purchasability, independently of notification success.
	lastOK bool
}

var _ runner.Script = (*Script)(nil)

// New returns a ready-to-use *Script (its documented zero value).
func New() *Script { return &Script{} }

// Name returns the stable identifier used for this script's logs and results.
func (*Script) Name() string { return "nintendo" }

// Schedule requests a run every minute using the @every syntax.
func (*Script) Schedule() string { return "@every 1m" }

// Run checks the product and notifies only on a transition into purchasable.
// Failed checks are not observations and leave the last known state intact.
func (s *Script) Run(ctx context.Context, rc *runner.Context) error {
	ok, checkErr := s.check(ctx, rc)
	rc.Log.InfoContext(ctx, "nintendo check completed", "purchasable", ok, "error", checkErr)
	if checkErr != nil {
		return checkErr
	}
	changed := s.hasRun && !s.lastOK && ok
	// Record the observation before sending: a failed notification must not
	// cause retries on steady states or swallow a later genuine transition.
	s.hasRun, s.lastOK = true, ok
	if changed {
		if err := rc.Notify(ctx, "Electric OCARINA OF TIME is now purchasable: "+productURL); err != nil {
			return errors.Join(checkErr, fmt.Errorf("notify nintendo transition: %w", err))
		}
	}
	return checkErr
}

func (*Script) check(ctx context.Context, rc *runner.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, productURL, nil) //nolint:gosec // G704 false positive: the fixed Nintendo product URL is the required monitoring target, not user input
	if err != nil {
		return false, fmt.Errorf("build nintendo request: %w", err)
	}
	// Use the injected client for its timeout and replaceable transport.
	resp, err := rc.HTTP.Do(req) //nolint:gosec // G704 false positive: fetching the fixed Nintendo product URL is the required stock-check behavior, not an SSRF vector
	if err != nil {
		return false, fmt.Errorf("perform nintendo request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			rc.Log.WarnContext(ctx, "close nintendo response body", "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("nintendo check: unexpected HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("read nintendo response: %w", err)
	}
	return purchasable(body)
}

var linkedData = regexp.MustCompile(`(?is)<script\b[^>]*\btype\s*=\s*["']application/ld\+json["'][^>]*>(.*?)</script\s*>`)

func purchasable(body []byte) (bool, error) {
	// The real product page publishes @graph Product (sku 129987) offers with
	// schema.org availability in server-rendered JSON-LD. This is more stable
	// than CSS/button copy and avoids unrelated "Add to cart" text. Select the
	// exact SKU, not another product's offer. Only InStock means buyable; absent
	// or unfamiliar data is a check error, never an invented stock transition.
	for _, match := range linkedData.FindAllSubmatch(body, -1) {
		var doc struct {
			Graph []json.RawMessage `json:"@graph"`
		}
		if err := json.Unmarshal(match[1], &doc); err != nil {
			continue
		}
		for _, node := range doc.Graph {
			var product struct {
				SKU    string `json:"sku"`
				Offers struct {
					Availability string `json:"availability"`
				} `json:"offers"`
			}
			// The observed graph also contains an array of ImageObjects.
			if err := json.Unmarshal(node, &product); err != nil || product.SKU != "129987" {
				continue
			}
			switch product.Offers.Availability {
			case "https://schema.org/InStock":
				return true, nil
			case "https://schema.org/OutOfStock", "https://schema.org/SoldOut", "https://schema.org/Discontinued":
				return false, nil
			default:
				return false, fmt.Errorf("nintendo check: unknown availability %q", product.Offers.Availability)
			}
		}
	}
	return false, errors.New("nintendo check: product availability not found")
}

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
const maxBodySize = 1 << 20

var linkedDataScripts = regexp.MustCompile(`(?is)<script\b[^>]*\btype\s*=\s*["']application/ld\+json["'][^>]*>(.*?)</script\s*>`)

// Script checks whether the Nintendo Electric OCARINA OF TIME is purchasable.
// Its zero value is ready to use. Reuse the same instance for successive runs;
// state is in memory only and resets when the process restarts. Run calls on an
// instance must be serial, not concurrent.
type Script struct {
	// hasRun distinguishes an initial observation from a previous failure.
	hasRun bool
	// lastOK belongs to the script, not the runner: here success means in stock.
	lastOK bool
}

// Keep this assertion so signature drift becomes a compile error instead of a
// surprise when the script is registered later.
var _ runner.Script = (*Script)(nil)

// New returns a ready-to-use *Script (its documented zero value).
func New() *Script { return &Script{} }

// Name returns the stable identifier used for this script's logs and results.
func (*Script) Name() string { return "nintendo" }

// Schedule requests a run every minute using the @every syntax.
func (*Script) Schedule() string { return "@every 1m" }

// Run checks the product, logs the outcome, and notifies only on becoming purchasable.
// A nil error means the item is purchasable and any required notification succeeded.
func (s *Script) Run(ctx context.Context, rc *runner.Context) error {
	checkErr := s.check(ctx, rc)
	ok := checkErr == nil
	rc.Log.InfoContext(ctx, "nintendo check completed", "ok", ok)

	changed := s.hasRun && s.lastOK != ok
	// Record the observed state even if notification fails. This script reports
	// transitions, not repeated alerts or notification retries on steady states.
	s.hasRun, s.lastOK = true, ok
	if changed && ok {
		msg := fmt.Sprintf("nintendo item is purchasable: %s", productURL)
		if err := rc.Notify(ctx, msg); err != nil {
			// Preserve causes for callers using errors.Is or errors.As.
			return errors.Join(checkErr, fmt.Errorf("notify nintendo transition: %w", err))
		}
	}
	return checkErr
}

func (*Script) check(ctx context.Context, rc *runner.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, productURL, nil) //nolint:gosec // G704 false positive: this monitoring script fetches a fixed operator-known Nintendo product URL, not user input
	if err != nil {
		return fmt.Errorf("build nintendo request: %w", err)
	}
	// Always use the supplied client: it owns timeouts and transport settings,
	// and tests can replace its transport without making any network calls.
	resp, err := rc.HTTP.Do(req) //nolint:gosec // G704 false positive: fetching the fixed operator-known Nintendo product URL is the required monitoring behavior, not an SSRF vulnerability
	if err != nil {
		return fmt.Errorf("perform nintendo request: %w", err)
	}
	defer func() {
		// A cleanup error is diagnostic; it does not change the observed state.
		if err := resp.Body.Close(); err != nil {
			rc.Log.WarnContext(ctx, "close nintendo response body", "error", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("nintendo check: unexpected HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize+1))
	if err != nil {
		return fmt.Errorf("read nintendo response: %w", err)
	}
	if len(body) > maxBodySize {
		return errors.New("nintendo response exceeds size limit")
	}

	// The fetched product page publishes JSON-LD @graph entries, including SKU
	// 129987 with offers.availability=https://schema.org/OutOfStock. Require that
	// SKU's offer to say InStock: HTTP 200 alone also describes unavailable items,
	// and page-wide matches can mistake recommended products for this item.
	// Unknown/missing availability (including PreOrder) fails closed.
	for _, match := range linkedDataScripts.FindAllSubmatch(body, -1) {
		var data struct {
			Graph []json.RawMessage `json:"@graph"`
		}
		if err := json.Unmarshal(match[1], &data); err != nil {
			return fmt.Errorf("decode nintendo linked data: %w", err)
		}
		for _, node := range data.Graph {
			var product struct {
				SKU    string `json:"sku"`
				Offers struct {
					Availability string `json:"availability"`
				} `json:"offers"`
			}
			// The live graph also contains arrays of gallery ImageObjects.
			if err := json.Unmarshal(node, &product); err != nil || product.SKU != "129987" {
				continue
			}
			if product.Offers.Availability == "https://schema.org/InStock" {
				return nil
			}
			return errors.New("nintendo item is not purchasable")
		}
	}
	return errors.New("nintendo product availability not found")
}

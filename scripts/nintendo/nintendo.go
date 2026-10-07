// Package nintendo monitors the Nintendo Store's Electric OCARINA OF TIME.
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
const maxResponseBytes = 1 << 20

var linkedData = regexp.MustCompile(`(?is)<script\b[^>]*\btype\s*=\s*["']application/ld\+json["'][^>]*>(.*?)</script\s*>`)

// Script notifies only when the product becomes purchasable. Its zero value is
// ready to use. Reuse the same instance for successive runs; state is in memory
// only and resets on restart. Run calls on an instance must be serial.
type Script struct {
	hasRun          bool
	lastPurchasable bool
}

var _ runner.Script = (*Script)(nil)

// New returns a ready-to-use *Script (its documented zero value).
func New() *Script { return &Script{} }

// Name returns the stable identifier used for logs and results.
func (*Script) Name() string { return "nintendo-ocarina" }

// Schedule requests a run every minute using the @every syntax.
func (*Script) Schedule() string { return "@every 1m" }

// Run fetches the product and notifies only on a transition into purchasable.
func (s *Script) Run(ctx context.Context, rc *runner.Context) error {
	purchasable, checkErr := s.check(ctx, rc)
	rc.Log.InfoContext(ctx, "nintendo check completed", "purchasable", purchasable, "error", checkErr)
	// A failed check is not a stock observation. Keep the baseline so recovery
	// from a transport or parsing error cannot produce a false restock alert.
	if checkErr != nil {
		return checkErr
	}
	changed := s.hasRun && s.lastPurchasable != purchasable
	// Record observations even if notification fails: no steady-state retries.
	s.hasRun, s.lastPurchasable = true, purchasable
	if changed && purchasable {
		if err := rc.Notify(ctx, "Electric OCARINA OF TIME is purchasable: "+productURL); err != nil {
			return errors.Join(checkErr, fmt.Errorf("notify nintendo transition: %w", err))
		}
	}
	return checkErr
}

func (*Script) check(ctx context.Context, rc *runner.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, productURL, nil)
	if err != nil {
		return false, fmt.Errorf("build nintendo request: %w", err)
	}
	resp, err := rc.HTTP.Do(req) //nolint:gosec // G704 false positive: fixed monitoring URL for the Nintendo product, not user-controlled input
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return false, fmt.Errorf("read nintendo response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return false, errors.New("nintendo response exceeds 1 MiB")
	}
	return purchasableFromPage(data)
}

func purchasableFromPage(data []byte) (bool, error) {
	// The plain product HTML embeds JSON-LD with @graph -> Product (sku
	// 129987) -> offers.availability. Only that product's explicit schema.org
	// InStock signal means purchasable; unrelated products and cart text do not.
	// Observed on the live page: https://schema.org/OutOfStock.
	for _, match := range linkedData.FindAllSubmatch(data, -1) {
		var document struct {
			Graph []json.RawMessage `json:"@graph"`
		}
		if err := json.Unmarshal(match[1], &document); err != nil {
			return false, fmt.Errorf("parse nintendo JSON-LD: %w", err)
		}
		for _, node := range document.Graph {
			var product struct {
				SKU    string `json:"sku"`
				Offers struct {
					Availability string `json:"availability"`
				} `json:"offers"`
			}
			// Nintendo's graph also includes arrays of ImageObjects.
			if err := json.Unmarshal(node, &product); err != nil || product.SKU != "129987" {
				continue
			}
			switch product.Offers.Availability {
			case "https://schema.org/InStock":
				return true, nil
			case "https://schema.org/OutOfStock":
				return false, nil
			default:
				return false, fmt.Errorf("unrecognized nintendo availability %q", product.Offers.Availability)
			}
		}
	}
	return false, errors.New("nintendo product availability missing from JSON-LD")
}

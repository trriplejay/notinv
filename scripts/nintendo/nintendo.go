// Package nintendo monitors the Electric OCARINA OF TIME on two storefronts:
// the Nintendo store offer and Target's ship-to-home availability.
package nintendo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/trriplejay/notinv/internal/runner"
)

const productURL = "https://www.nintendo.com/us/store/products/electric-ocarina-of-time-129987/"

// targetURL is the Target product page linked in notifications.
const targetURL = "https://www.target.com/p/hori-electric-ocarina-of-time-nintendo-switch-2/-/A-1013288947"

// targetTCIN is Target's item number for the product.
const targetTCIN = "1013288947"

// redskyURL is Target's public fulfillment service for the product. Every
// parameter is fixed: key is the public page key embedded in Target's product
// page (not a secret), and the store/zip pin the location Target itself served
// the page with, so no configuration or geolocation is involved. Only the
// ship-to-home status in the response is read, which is location independent.
const redskyURL = "https://redsky.target.com/redsky_aggregations/v1/web/product_fulfillment_v1" +
	"?key=9f36aeafbe60771e321a7cc95a78140772ab3e96&is_bot=false&tcin=" + targetTCIN +
	"&store_id=2454&required_store_id=2454&scheduled_delivery_store_id=2454" +
	"&zip=51503&state=IA&paid_membership=false&base_membership=false&card_membership=false" +
	"&channel=WEB&page=%2Fp%2FA-" + targetTCIN

// browserUserAgent replaces the service's default User-Agent on the Target
// request, which Target's bot filter otherwise refuses more often.
const browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

const productName = "Electric OCARINA OF TIME"

// siteState is the last known availability of one storefront.
type siteState struct {
	// hasRun distinguishes the silent baseline from subsequent observations.
	hasRun bool
	// lastOK records availability, independently of notification success.
	lastOK bool
	// failing records that the latest check of this site failed, so the first
	// success afterwards is logged once as a recovery.
	failing bool
}

// observe applies one check result and reports whether the site newly became
// available. A failed check is not an observation and keeps the known state.
func (st *siteState) observe(ctx context.Context, rc *runner.Context, site string, ok bool, err error) bool {
	if err != nil {
		st.failing = true
		return false
	}
	if st.failing {
		st.failing = false
		rc.Log.InfoContext(ctx, site+" check recovered")
	}
	changed := st.hasRun && !st.lastOK && ok
	st.hasRun, st.lastOK = true, ok
	return changed
}

// Script checks both storefronts and notifies on each site's transition into
// stock. Its zero value is ready to use. Reuse the same instance for successive
// runs; state is in memory only and resets when the process restarts. Run calls
// on an instance must be serial, not concurrent.
type Script struct {
	nintendo siteState
	target   siteState
}

var _ runner.Script = (*Script)(nil)

// New returns a ready-to-use *Script (its documented zero value).
func New() *Script { return &Script{} }

// Name returns the stable identifier used for this script's logs and results.
func (*Script) Name() string { return "nintendo" }

// Schedule requests a run every 30 seconds using the @every syntax.
func (*Script) Schedule() string { return "@every 30s" }

// Run checks both sites independently and sends at most one notification,
// linking each site that newly became available. Each site's first successful
// read is a silent baseline. A failed check is reported in the returned error
// and leaves that site's last known state intact without affecting the other.
func (s *Script) Run(ctx context.Context, rc *runner.Context) error {
	nintendoOK, nintendoErr := s.check(ctx, rc)
	rc.Log.InfoContext(ctx, "nintendo check completed", "purchasable", nintendoOK, "error", nintendoErr)
	targetOK, targetErr := checkTarget(ctx, rc)
	rc.Log.InfoContext(ctx, "target check completed", "available", targetOK, "error", targetErr)
	checkErr := errors.Join(nintendoErr, targetErr)

	// Record the observations before sending: a failed notification must not
	// cause retries on steady states or swallow a later genuine transition.
	nintendoNew := s.nintendo.observe(ctx, rc, "nintendo", nintendoOK, nintendoErr)
	targetNew := s.target.observe(ctx, rc, "target", targetOK, targetErr)
	msg := restockMessage(nintendoNew, targetNew)
	if msg == "" {
		return checkErr
	}
	if err := rc.Notify(ctx, msg); err != nil {
		return errors.Join(checkErr, fmt.Errorf("notify restock: %w", err))
	}
	return checkErr
}

// restockMessage names the product with the link of each newly available
// site; a Nintendo-only restock keeps the original message. Empty means none.
func restockMessage(nintendoNew, targetNew bool) string {
	var parts []string
	if nintendoNew {
		parts = append(parts, "is now purchasable: "+productURL)
	}
	if targetNew {
		parts = append(parts, "is now available to ship from Target: "+targetURL)
	}
	if len(parts) == 0 {
		return ""
	}
	return productName + " " + strings.Join(parts, " and ")
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

func checkTarget(ctx context.Context, rc *runner.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, redskyURL, nil) //nolint:gosec // G704 false positive: the fixed Target fulfillment URL is the required monitoring target, not user input
	if err != nil {
		return false, fmt.Errorf("build target request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", browserUserAgent)
	// Use the injected client for its timeout and replaceable transport.
	resp, err := rc.HTTP.Do(req) //nolint:gosec // G704 false positive: fetching the fixed Target fulfillment URL is the required stock-check behavior, not an SSRF vector
	if err != nil {
		return false, fmt.Errorf("perform target request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			rc.Log.WarnContext(ctx, "close target response body", "error", err)
		}
	}()
	// Target answers 435 when its bot filter refuses a request; that and any
	// other non-200 status is a failed check, never an observation.
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("target check: unexpected HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("read target response: %w", err)
	}
	return targetAvailable(body)
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

// targetAvailable reads Target's ship-to-home status for the exact TCIN. Only
// data.product.fulfillment.shipping_options.availability_status decides; store
// pickup and scheduled delivery are ignored. Absent, mismatched or unfamiliar
// data is a check error, never an invented stock transition.
func targetAvailable(body []byte) (bool, error) {
	var doc struct {
		Data struct {
			Product struct {
				TCIN        string `json:"tcin"`
				Fulfillment struct {
					ShippingOptions struct {
						AvailabilityStatus string `json:"availability_status"`
					} `json:"shipping_options"`
				} `json:"fulfillment"`
			} `json:"product"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return false, fmt.Errorf("target check: decode response: %w", err)
	}
	product := doc.Data.Product
	if product.TCIN != targetTCIN {
		return false, fmt.Errorf("target check: response is for tcin %q, want %s", product.TCIN, targetTCIN)
	}
	switch status := product.Fulfillment.ShippingOptions.AvailabilityStatus; status {
	case "IN_STOCK":
		return true, nil
	case "OUT_OF_STOCK", "UNAVAILABLE", "DISCONTINUED":
		return false, nil
	case "":
		return false, errors.New("target check: shipping availability not found")
	default:
		return false, fmt.Errorf("target check: unknown shipping availability %q", status)
	}
}

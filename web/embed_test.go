package webassets

import (
	"bytes"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestEmbeddedAssetsStayLocal(t *testing.T) {
	external := regexp.MustCompile(`(?i)https?://`)
	// SVG requires this namespace identifier; it is never fetched.
	svgNamespace := []byte("http://www.w3.org/2000/svg")
	err := fs.WalkDir(assets, "static", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := assets.ReadFile(path)
		if err != nil {
			return err
		}
		if external.Match(bytes.ReplaceAll(data, svgNamespace, nil)) {
			t.Errorf("external URL literal in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"static/vendor/chart.umd.min.js", "static/vendor/chartjs-adapter-date-fns.bundle.min.js"} {
		data, err := assets.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) < 40000 {
			t.Errorf("%s is not a full library distribution (%d bytes)", path, len(data))
		}
	}
}

// TestLandingPageRefreshDashboardQueueAndImmediateToggle ensures web/static/index.html
// updates the card's toggle button and data-enabled attribute immediately on response,
// and queues overlapping refreshDashboard() calls via refreshQueued rather than dropping them.
func TestLandingPageRefreshDashboardQueueAndImmediateToggle(t *testing.T) {
	data, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	// Must declare refreshQueued state variable
	if !strings.Contains(content, "let refreshQueued = false;") {
		t.Error("index.html missing 'let refreshQueued = false;' declaration")
	}

	// Must set refreshQueued = true when refresh is already in flight
	if !strings.Contains(content, "if (refreshing) {\n        refreshQueued = true;\n        return;\n      }") {
		t.Error("index.html refreshDashboard missing refreshQueued = true on concurrent call")
	}

	// Must check and drain refreshQueued in finally block
	if !strings.Contains(content, "if (refreshQueued) {\n          refreshQueued = false;\n          void refreshDashboard();\n        }") {
		t.Error("index.html refreshDashboard missing queued refresh drain in finally block")
	}

	// Must immediately update toggleBtn label and dataset.enabled from response
	if !strings.Contains(content, "toggleBtn.textContent = data.enabled ? 'Disable' : 'Enable';") ||
		!strings.Contains(content, "element.dataset.enabled = String(data.enabled);") {
		t.Error("index.html toggle click missing immediate update of toggleBtn text and element.dataset.enabled")
	}
}

// TestScriptPageRefreshDashboardQueueAndImmediateToggle ensures web/static/script.html
// updates the toggle button and data-enabled attribute immediately on response,
// and queues overlapping refreshDashboard() calls via refreshQueued rather than dropping them.
func TestScriptPageRefreshDashboardQueueAndImmediateToggle(t *testing.T) {
	data, err := assets.ReadFile("static/script.html")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	// Must declare refreshQueued state variable
	if !strings.Contains(content, "let refreshQueued = false;") {
		t.Error("script.html missing 'let refreshQueued = false;' declaration")
	}

	// Must set refreshQueued = true when refresh is already in flight
	if !strings.Contains(content, "if (refreshing) {\n        refreshQueued = true;\n        return;\n      }") {
		t.Error("script.html refreshDashboard missing refreshQueued = true on concurrent call")
	}

	// Must check and drain refreshQueued in finally block
	if !strings.Contains(content, "if (refreshQueued) {\n          refreshQueued = false;\n          void refreshDashboard();\n        }") {
		t.Error("script.html refreshDashboard missing queued refresh drain in finally block")
	}

	// Must immediately update toggleButton label and card.element.dataset.enabled from response
	if !strings.Contains(content, "toggleButton.textContent = data.enabled ? 'Disable' : 'Enable';") ||
		!strings.Contains(content, "if (card) card.element.dataset.enabled = String(data.enabled);") {
		t.Error("script.html toggle click missing immediate update of toggleButton text and card.element.dataset.enabled")
	}
}


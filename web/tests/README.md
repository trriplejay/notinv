# Dashboard tests

The Go suite (`go test -race ./...`) checks embedded root/vendor responses from an
empty working directory, unknown-path 404s, all API routes and healthz on the real
mux, and the absence of external URL literals in every embedded asset.

The browser suite runs real Chromium + Chart.js/date-fns against the embedded page,
real Go API handlers and a throwaway Postgres database. The fixture process is opt-in
and does not run during ordinary Go tests. Fixture data exists only in tests.

From the repository root, start the assembly in one terminal (`NOTINV_TEST_DATABASE_URL`
must name a reachable throwaway Postgres database):

```sh
docker run --rm --name notinv-dashboard-browser -p 127.0.0.1:18080:18080 \
  -v "$PWD":/src -w /src -e NOTINV_BROWSER_TEST_LISTEN=:18080 \
  -e NOTINV_TEST_DATABASE_URL \
  golang:1.26 go test ./cmd/notinv -run '^TestDashboardBrowserServer$' -v -timeout 20m
```

Wait for `browser assembly ready`, then in another terminal:

```sh
npm --prefix web/tests ci --ignore-scripts
docker run --rm --network host -v "$PWD":/src -w /src/web/tests \
  mcr.microsoft.com/playwright:v1.58.2-noble npm test
docker stop notinv-dashboard-browser
```

The host-network example targets Linux. With locally installed Playwright Chromium,
`npm --prefix web/tests test` also works. `NOTINV_BROWSER_URL` overrides the default
`http://127.0.0.1:18080` test origin.

The per-script page scenarios are in `script-page.test.cjs`. With the same assembly
running, invoke them separately with `node --test script-page.test.cjs` from
`web/tests` (or replace `npm test` in the Playwright container command above).
They cover linked slim cards, detail fields, chart ranges, direct navigation,
missing names, and failure messages. The older `dashboard.test.cjs` still expects
full-detail landing cards and is not the gate for the slim-card layout.

The component-level enable/disable toggle tests are in `toggle.test.cjs`
(`npm --prefix web/tests run test:toggle` or `node --test toggle.test.cjs`).
They test the real static pages (`web/static/index.html` and `web/static/script.html`)
with Playwright without requiring the Go server or Postgres database. Static assets
are served from disk and API routes (`/api/scripts`, `/api/scripts/*/enabled`,
and series endpoints) are intercepted via `page.route`. Tests cover landing card
and script page button state flipping (CLM-1, CLM-2), 500 and network failure
handling (CLM-3), debouncing/disabled state while requests are pending (CLM-4),
not-found script handling (CLM-5), and periodic 30-second refresh relabeling (CLM-6).

Tests cover live DTO cards (including null latest and untrusted text), time-axis
URL grouping, status/error colors, all range windows through the RFC3339 parser,
auto-refresh with a virtual browser clock, dynamic dark mode, no external requests,
and component-level empty/error/recovery states with only the API substituted.

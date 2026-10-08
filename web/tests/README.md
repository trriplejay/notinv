# Dashboard tests

The Go suite (`go test -race ./...`) checks embedded root/vendor responses from an
empty working directory, unknown-path 404s, all API routes and healthz on the real
mux, and the absence of external URL literals in every embedded asset.

The browser suite runs real Chromium + Chart.js/date-fns against the embedded page,
real Go API handlers and a temporary SQLite database. The fixture process is opt-in
and does not run during ordinary Go tests. Fixture data exists only in tests.

From the repository root, start the assembly in one terminal:

```sh
docker run --rm --name notinv-dashboard-browser -p 127.0.0.1:18080:18080 \
  -v "$PWD":/src -w /src -e NOTINV_BROWSER_TEST_LISTEN=:18080 \
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

Tests cover live DTO cards (including null latest and untrusted text), time-axis
URL grouping, status/error colors, all range windows through the RFC3339 parser,
auto-refresh with a virtual browser clock, dynamic dark mode, no external requests,
and component-level empty/error/recovery states with only the API substituted.

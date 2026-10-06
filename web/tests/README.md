# Dashboard browser tests

The component tests run the embedded page, real Chart.js and real date adapter
in Chromium, substituting only API responses. The assembly test runs without
request interception against the real Go API and an isolated, seeded SQLite
store. Test data and dependencies are outside `web/static`, so they are not
embedded in the production binary.

From the repository root, start the test server (Go 1.26 with CGO):

```sh
go run ./web/tests/server
```

In another terminal:

```sh
cd web/tests
npm ci
npx playwright install --with-deps chromium
npm test
```

The server listens on `127.0.0.1:4173`. Tests can use another externally started
instance of this test server via `DASHBOARD_URL`. Do not point them at a production
instance: the assembly test expects the seeded database. Stop the server after
testing; its database is created in the operating system's temporary directory.

Go embedded-file HTTP tests run with `go test ./web`. Browser tests additionally
cover cards, escaped API strings, URL grouping, time-axis rendering, status
colors, RFC3339 windows, automatic refresh, dark mode, empty/error responses and
same-origin-only asset requests.

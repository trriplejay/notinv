# notinv

A long-running Go service that runs periodic website checks (cron-style), records simple
stats for each run (timestamp, duration in ms, HTTP status code), sends Discord direct
messages when a check's state changes, and serves a single dashboard page.

> Status: early scaffolding. See the GitHub issues for the roadmap.

## Layout

```
cmd/notinv/          entrypoint
internal/config/     config loading and validation
internal/checks/     Check interface and built-in check types
internal/scheduler/  cron-style scheduling of checks
internal/store/      SQLite persistence of results
internal/notify/     Discord DM notifications
internal/web/        dashboard + JSON API server
web/static/          dashboard frontend assets (embedded into the binary)
```

## Development

Requires Go 1.26+. Linting requires [golangci-lint](https://golangci-lint.run/welcome/install/) v2.

```sh
make build   # build ./bin/notinv
make run     # build and run (Ctrl-C to stop)
make test    # go test -race ./...
make lint    # golangci-lint run
```

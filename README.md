# notinv

A long-running Go service that runs periodic website checks (cron-style), records
run timestamps, durations and success/error results, sends Discord direct messages
when a script detects a state change, and serves a single dashboard page.

Follow the sections below to clone the repo, configure Discord and a database,
copy and register your own script, run the service, and trigger your first DM.

## 1. Clone and prepare

You need Git and either Go 1.26+ with Make and a C compiler (the build enables CGO),
or Docker with the Compose plugin. For real notifications, you also need a Discord
account and a server where you can invite a bot. Turso is optional.

```sh
git clone https://github.com/trriplejay/notinv.git
cd notinv
cp .env.example .env
```

Run the commands below from this repository root. Edit `.env` with your settings;
never commit bot tokens, database tokens, or other real credentials.

## 2. Discord setup

1. Open the [Discord Developer Portal](https://discord.com/developers/applications),
   choose **New Application**, and give it a name.
2. Open the application's **Bot** page. Create a bot if prompted, then use
   **Reset Token** to obtain its bot token. Copy that token into
   `NOTINV_DISCORD_TOKEN` in `.env`. Use the **bot token**, not the application ID,
   public key, or OAuth client secret. Treat it as a password.
3. Under **OAuth2 → URL Generator**, select the `bot` scope. Open the generated
   invite URL and authorize the bot in a server you belong to and can manage.
   The bot and your user must share a server; administrator permission is not
   needed for this DM-only use. Allow direct messages from that server's members
   in Discord's privacy settings, and make sure you have not blocked the bot.
4. In the Discord client, enable **User Settings → Advanced → Developer Mode**.
   Right-click your own user in the server's member list and choose **Copy User ID**.
   Put that numeric ID in `NOTINV_DISCORD_USER_ID` in `.env`. This is your **user**
   ID, not the bot, server, or channel ID.

Set **both** `NOTINV_DISCORD_TOKEN` and `NOTINV_DISCORD_USER_ID` to receive DMs.
Leaving **both empty** runs notifications in **dry-run mode**: the service logs a
startup warning and logs notification messages instead of contacting Discord.
Supplying only one of the two is a startup error.

## 3. Database setup: Turso or a local file

Choose one of these paths. The service opens the database and applies its schema
migrations at startup; you do not need to create tables by hand.

### Turso

Install the [Turso CLI](https://docs.turso.tech/cli/installation) and sign in to your
Turso account. Create a database (here named `notinv`), then retrieve its URL and
an auth token:

```sh
turso auth login
turso db create notinv
turso db show --url notinv
turso db tokens create notinv
```

Copy the `libsql://...` URL printed by `turso db show --url` into `DATABASE_URL`
in `.env`. Copy the token printed by `turso db tokens create` into
`DATABASE_AUTH_TOKEN`. A `libsql://` URL **requires a nonempty auth token**;
the service rejects that configuration without one. Keep the token private.
Turso stores your data remotely, so it needs no local database directory or volume.

### Local file (no Turso needed)

Keep these values from `.env.example`:

```dotenv
DATABASE_URL=file:./data/notinv.db
DATABASE_AUTH_TOKEN=
```

`file:./data/notinv.db` is also the default when `DATABASE_URL` is unset. No auth
token is needed. For a local run, create the parent directory and ensure it is
writable by the user running notinv:

```sh
mkdir -p data
```

The relative path is resolved from the service's working directory. For container
persistence, see the volume instructions in the Compose section below.

## 4. Environment variable reference

These are the **eight central service settings**, matching `.env.example` and
`internal/config/config.go`. Script-specific settings are separate (see below).

| Variable | Default when unset | Meaning / validation |
| --- | --- | --- |
| `NOTINV_LISTEN` | `:8080` | HTTP listen address for the dashboard and API. |
| `DATABASE_URL` | `file:./data/notinv.db` | Database URL; supported schemes are `file:` and `libsql:`. |
| `DATABASE_AUTH_TOKEN` | Empty | Required for a `libsql:` URL; not needed for a local file. |
| `NOTINV_DISCORD_TOKEN` | Empty | Discord bot token; set together with the recipient user ID. |
| `NOTINV_DISCORD_USER_ID` | Empty | Your Discord user ID; both Discord settings empty selects dry-run. |
| `NOTINV_RETENTION_DAYS` | `30` | Days of history to retain; must be a positive integer. |
| `NOTINV_LOG_LEVEL` | `info` | One of `debug`, `info`, `warn`, `error`. |
| `NOTINV_LOG_FORMAT` | `text` | One of `text`, `json`. |

Defaults apply to **unset** variables, not explicitly empty values. The service
loads `.env` by default; existing process environment variables take precedence
over file values. To select another dotenv file after building, run:

```sh
./bin/notinv -env-file path/to/file
```

An absent default `.env` is allowed by the binary (for example, when settings are
injected into a container). An explicitly selected missing file is a startup
error, including `-env-file .env`. Compose separately requires the `.env` file
named in its `env_file` setting. `./bin/notinv -version` prints the build version
without loading dotenv or validating configuration.

## 5. Write and register your own script

### Copy the example

Start with the working HTTP-check implementation and its tests:

```sh
cp -R scripts/example scripts/widget
```

In the copied directory, change `package example` to `package widget` in **all Go
files**, including `doc.go` and `example_test.go`. Replace the script name
`example` with `widget` in names, messages, comments and test expectations, and
replace `EXAMPLE_URL` with `WIDGET_STOCK_URL` in the implementation and tests.
The filenames can stay unchanged; Go uses the package declaration.

The original `scripts/example/example.go` supplies these signatures (shown here
as a signature reference, not a replacement implementation):

```go
func New() *Script
func (*Script) Name() string
func (*Script) Schedule() string
func (s *Script) Run(ctx context.Context, rc *runner.Context) error
```

`runner.Script` requires `Name`, `Schedule`, and `Run`; `New() *Script` is the
constructor convention, not an interface method. Keep the compile-time check:

```go
var _ runner.Script = (*Script)(nil)
```

Give `Name()` a unique, stable identifier (`"widget"`). `Schedule()` accepts a
five-field cron expression or `@every <duration>`; the template uses `"@every 5m"`.
Each script runs once immediately at startup, then on its schedule. For quicker
follow-up checks while testing DMs you can change it to `"@every 10s"`, then
restore your preferred interval afterward.

### Implement the check and notification policy

For your first script, keep the copied check logic: it performs an HTTP GET and
considers exactly HTTP 200 healthy. Later you can adapt `check` to inspect page
content or JSON, or implement another success condition. `Run` returns `nil` on
success and an error on failure; the runner records that outcome.

Use the shared services in `*runner.Context`:

- `rc.Log` (`*slog.Logger`) for structured logging, e.g.
  `rc.Log.InfoContext(ctx, "widget check completed", "ok", ok)`.
- `rc.HTTP` (`*http.Client`) for HTTP requests, using `rc.HTTP.Do(req)` rather than
  a separate client. Build requests with `http.NewRequestWithContext` and close
  response bodies as the example does. The entrypoint supplies a 30-second timeout.
- `rc.Notify(ctx, msg)` (`func(context.Context, string) error`) to send a Discord
  DM, or log the message in dry-run mode. Handle the returned error.

**The script owns state-change detection and calls `rc.Notify`; returning an
error alone does not send a DM.** Keep the example's `hasRun` / `lastOK` logic to
notify only when healthy/unhealthy state changes. The first observation sets a
baseline with no notification. State is in memory and resets on service restart;
runs on each instance are serial. The template records the new state even if
notification delivery fails, so a steady state does not trigger further attempts
on subsequent runs.

### Script variables

Scripts read their own variables directly with `os.Getenv`, using a script-name
prefix: `SCRIPTNAME_VAR`. The original template reads `os.Getenv("EXAMPLE_URL")`;
your renamed `widget` script reads `os.Getenv("WIDGET_STOCK_URL")`. These are
per-script settings, **not additions to the eight central configuration keys**.

Add `WIDGET_STOCK_URL` to your `.env`, setting it to the full HTTP(S) URL of an
endpoint you control whose response you can change. It must be reachable from
where notinv runs; inside a container, `localhost` means the container itself.
Set script variables before startup. Editing `.env` does not reload a running
process; restart for configuration changes, but change the **endpoint's response**
without restarting notinv when testing state transitions.

### Register in the entrypoint

`cmd/notinv/main.go` currently has this one-line registration:

```go
scripts := []runner.Script{example.New()}
```

For this walkthrough, **replace** the example import with
`"github.com/trriplejay/notinv/scripts/widget"` and replace the registration line
with:

```go
scripts := []runner.Script{widget.New()}
```

That keeps only your new script running and avoids an unused `example` import.
To run both instead, keep both imports and use
`scripts := []runner.Script{example.New(), widget.New()}`; also set `EXAMPLE_URL`
in `.env` for the original script. Registration is explicit, not directory
scanning. Rebuild after changing script code or registration.

## 6. Run the service

Choose one path below, with `.env` configured and your script registered.

### Locally

```sh
make build   # build ./bin/notinv
make run     # build and run (Ctrl-C to stop)
```

Run from the repo root so `.env` and the local database path resolve as described.
With the default listen address, open <http://localhost:8080> for the dashboard.

### Docker Compose

From the repo root, with Docker running:

```sh
docker compose up
```

The repository's `docker-compose.yml` defines the `notinv` service, builds from
the local `Dockerfile`, loads `.env` through `env_file: .env`, and publishes
`8080:8080` (host port 8080 to container port 8080). Keep `NOTINV_LISTEN=:8080`
for that mapping, and open <http://localhost:8080>. After modifying script code
or registration, use `docker compose up --build` to rebuild the image.

For `DATABASE_URL=file:./data/notinv.db`, the Compose file includes a
**commented-out** `./data:/app/data` volume. To keep the database across container
replacement, create `./data` on the host, make it writable by the container's
nonroot user, and uncomment the `volumes:` block and that mount in
`docker-compose.yml`. Without the mount, local database data lives in the
container and is lost when the container is replaced. A remote libsql/Turso
database stores data remotely and needs **no volume**; leave the block commented.

### Hot reload (Docker)

```sh
make dev
```

Runs the service in a development container (`Dockerfile.dev`,
`docker-compose.dev.yml`) with the repo mounted and [air](https://github.com/air-verse/air)
watching it: saving a `.go` file or anything under `web/static` rebuilds and
restarts the service. `.env` is loaded as usual, except `DATABASE_URL` is forced
to `file:/tmp/notinv.db` inside the container, so the database is discarded when
you stop `make dev` (Ctrl-C) and the container is removed. Only the Go build and
module caches are kept in named volumes so rebuilds stay fast; remove them with
`docker compose -f docker-compose.dev.yml down -v`.

## 7. Trigger your first Discord DM

1. Make the endpoint in `WIDGET_STOCK_URL` return HTTP 200, then start notinv using
   one of the paths above. Ensure both Discord credentials are set (not dry-run).
2. The first check runs right away at startup. Confirm a successful run in the
   dashboard or logs. **No DM on this first baseline run is expected.**
3. Without restarting notinv, change that same endpoint to return a non-200
   status, such as HTTP 503, and leave it that way until the next scheduled
   check (five minutes with the template schedule, or about ten seconds with
   `@every 10s`). The
   copied script detects healthy → unhealthy and calls `rc.Notify`, sending
   `widget state changed: ok=false` to your configured Discord user.
4. Restore HTTP 200 and wait for another check to receive
   `widget state changed: ok=true`. Unchanged states produce no extra DMs.

If no DM arrives, check for the dry-run startup warning, confirm your bot token
and **user** ID, shared server and DM privacy settings, and inspect run errors in
the logs/dashboard. Fix delivery issues, then cause another observed state
transition: the template does not resend an unchanged state after a failed send.
Keep notinv running across the baseline and transition; restarting clears the
script's remembered state. Test against an endpoint you control, not by disrupting
someone else's service.

## Layout

```text
cmd/notinv/          entrypoint and explicit script registration
scripts/example/    copyable script implementation and tests
internal/config/    config loading and validation
internal/runner/    Script interface, shared Context, scheduling and run recording
internal/store/     database persistence and migrations
internal/notify/    Discord DM notifications
internal/web/       dashboard JSON API handlers
web/static/         dashboard frontend assets (embedded into the binary)
```

## Development

Requires Go 1.26+ and a C compiler for the CGO-enabled build. Linting requires
[golangci-lint](https://golangci-lint.run/welcome/install/) v2 (CI uses v2.14.0).

```sh
make build   # build ./bin/notinv
make run     # build and run (Ctrl-C to stop)
make dev     # hot-reloading Docker container with a throwaway database
make test    # go test -race ./...
make lint    # golangci-lint run ./...
make fmt     # format Go source with gofmt
```

Keep the copied script tests aligned with your script's behavior, particularly
its initial baseline, healthy/unhealthy transitions, HTTP failures and notification
errors. See the GitHub issues for ongoing development and the roadmap.

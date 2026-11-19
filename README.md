# Blackbox

Blackbox is a self-hosted log store for a Docker server: it collects every container's output, keeps it in Postgres for 30 days, and makes it searchable from a command line and an authenticated API.

## Features

- **Every container, nothing to set up.** Blackbox reads each container's log from Docker, so a project doesn't have to change anything to be collected. A container opts out with the label `com.mobasity.logs=off`.
- **Logs survive the container.** Lines are kept after a container is replaced or removed, for 30 days or until they take 1 GB, oldest first.
- **Structured where it can be.** JSON lines, Go's `slog` text format and Go's `log` package are read into a level, a message and fields where they have them. Anything else is kept as plain text. Common secrets (bearer tokens, passwords in URLs, authorization and cookie headers, and fields named like passwords, secrets, tokens or API keys) are redacted before a line is stored.
- **Search.** Find lines by time, project, service, container, version, level, stream, text or field, and read the lines around any line, for a stack trace after a panic.
- **Repeated errors.** Error and warning lines are counted per project per hour or day, and grouped by fingerprint: the message with its ids and numbers taken out, so the same problem counts once however often it repeats.
- **It says what it missed.** Lines that were lost, or may have been, are recorded as gaps: a container removed before it was read to the end, the collector not running, or a Docker log damaged, for example by a power cut.
- **Read-only, with one token per consumer.** The API only reads, and each program that uses it has its own bearer token.

[blackboxlog](https://github.com/lsariol/blackboxlog) is a Go package for logs Blackbox reads well. Using it is optional: every container is collected either way.

Blackbox is for diagnostic logs. It isn't a place for records that must not be lost: those belong in an application's own database.

### How it works

One Go binary runs as three containers:

| Container | What it does |
|---|---|
| `blackbox-proxy` | The only part with the Docker socket. It has no network: it lists containers and passes their log bytes through a Unix socket without reading them. |
| `blackbox-collector` | Follows every container's log through the proxy, parses and redacts each line, saves it to Postgres, and applies the database migrations. |
| `blackbox-api` | Serves searches, error counts and collection status to authenticated consumers on port 3190, and runs the CLI. |

A line counts as read once its transaction commits, together with its container's checkpoint. There's no retry queue: after any failure, the collector reads again from Docker's log files.

| Event | Result |
|---|---|
| Collector restart, database or proxy outage | It resumes from the checkpoints: nothing is lost and nothing is stored twice. |
| A container removed before it was read to the end | A `container_removed` gap. |
| The collector down for over a minute | A `collector_down` gap. |
| A damaged Docker log | Docker silently skips what's after the damage, so Blackbox checks for that, records a `log_damaged` gap and keeps collecting new lines. |

### The CLI

The CLI runs in the `blackbox-api` container, either as a shell or one command at a time:

```bash
docker exec -it blackbox-api /blackbox shell
docker exec blackbox-api /blackbox search --level error --since 6h
```

| Command | What it shows |
|---|---|
| `search [text] [flags]` | Lines by time, project, service, container, version, level, stream, text or field |
| `show <id>` | One line with every field |
| `context <id>` | The lines around a line |
| `errors` | Repeated errors, grouped, most frequent first |
| `status` | Collection health; it exits non-zero when something needs attention |
| `sources` | Every project and service with logs |
| `gaps` | Known and possible loss |
| `consumers`, `migrate status`, `version`, `help`, `exit` | Setup |

`help <command>` shows each command's flags and examples.

### The API

`blackbox-api` listens on port 3190 and isn't published: other containers reach it on the same Docker network. Every `/v1/` request needs `Authorization: Bearer <token>`.

| Endpoint | Returns |
|---|---|
| `GET /healthz` | Whether the API and its database are up, without a token |
| `GET /v1/logs` | A search, newest or oldest first, a page at a time |
| `GET /v1/logs/{id}` | One line with its fields and where it came from |
| `GET /v1/logs/{id}/context` | The lines around a line in the same container |
| `GET /v1/sources` | The projects, services and versions with logs |
| `GET /v1/gaps` | Known and possible loss in a time range |
| `GET /v1/status` | Collection health |
| `GET /v1/migrations` | The database migrations and when each was applied |
| `GET /v1/stats/errors` | Error and warning lines per project, per hour or day |
| `GET /v1/stats/errors/groups` | Repeated lines grouped by fingerprint, most first |

## Requirements

- Go 1.27.4
- Docker, to run the containers and the test stage
- PostgreSQL 16

## Getting started

1. Create the Docker network the containers join, if it doesn't exist: `docker network create spark`.
2. Create the database and its roles: run `scripts/db/setup.sql` with `psql` as a superuser, giving a password for each login role. It's safe to run again, and never changes the password of a role that exists.

   ```bash
   psql -U postgres -d postgres -v migrator_password=... -v app_password=... -v reader_password=... -f scripts/db/setup.sql
   ```

3. Copy `.env.example` to `.env` and fill it in: the three connection strings, as the containers reach Postgres, and a token of at least 32 random characters for each API consumer (`openssl rand -hex 32`).
4. Start it:

   ```bash
   docker compose -f docker-compose.yml -f docker-compose.dev.yml up --build
   ```

All three containers turn `healthy` in `docker ps`, and `docker exec blackbox-api /blackbox status` prints no "Needs attention" line. The development compose file publishes the API on `127.0.0.1:3191`.

## Configuration

Everything is read from the environment.

| Variable | Default | Meaning |
|---|---|---|
| `APP_ENV` | `prod` | `prod`, or `dev` for readable logs and a `(dev)` prompt |
| `BLACKBOX_VERSION` | `dev-<commit>` | The version it reports |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `TZ` | `UTC` | The time zone the CLI shows times in |
| `BLACKBOX_DATABASE_URL` | | The collector's connection string, as `blackbox_app` |
| `BLACKBOX_DATABASE_MIGRATOR_URL` | | The connection string that runs migrations, as `blackbox_migrator` |
| `BLACKBOX_DATABASE_READER_URL` | | The API's connection string, as `blackbox_reader` |
| `BLACKBOX_CLI_TOKEN` | | The CLI's API token, at least 32 characters |
| `SHARED_BLACKBOX_VISTA_TOKEN` | | Vista's API token, at least 32 characters |

The API doesn't start without every consumer's token. Consumers are defined in `internal/api/consumers.go`.

## Development

```bash
go test ./...                                              # the tests
docker build --target test .                               # gofmt, vet and the tests with -race, as before a deploy
scripts/test-db.sh up && eval "$(scripts/test-db.sh env)"  # a throwaway Postgres, so the database tests run too
scripts/test-db.sh down                                    # remove it
```

In Git Bash on Windows, set `MSYS_NO_PATHCONV=1` first, or `docker exec … /blackbox` has its path rewritten.

## Deployment

`docker-compose.yml` is written for [Lighthouse](https://github.com/lsariol/lighthouse), which deploys each version tag. It expects an external Docker network named `spark` and reads its secrets from the environment. `blackbox-proxy` mounts the Docker socket and runs as root to read it; the other two containers run as user 10001. Changes are listed in [CHANGELOG.md](CHANGELOG.md).

## License

See [LICENSE](LICENSE).

# Blackbox

Spark's private log store. It reads every container's output from Docker, keeps it in sparkdb for 30 days, so logs survive a container being replaced, and answers searches through an authenticated API. Its first consumer is its own CLI; Vista comes later on the same API.

Blackbox collects, stores and searches diagnostic logs. It never changes a container, and it isn't a place for records that must not be lost: those belong in a project's own database.

- **Changes:** [CHANGELOG.md](CHANGELOG.md).
- **The logging standard for other projects:** [blackboxlog](https://github.com/lsariol/blackboxlog). Using it is optional: every container is collected either way.

## Parts

One Go binary, run as three containers:

| Container | Does |
|---|---|
| `blackbox-proxy` | The only part with the Docker socket. No network; lists containers and passes log bytes through without reading them |
| `blackbox-collector` | Follows every container's log, parses and redacts each line, saves it to sparkdb, and runs the migrations |
| `blackbox-api` | Serves searches and collection status to authenticated consumers on `blackbox-api:3190` (the `spark` network) |

## What it does when things go wrong

A line counts as read once its transaction commits, together with its container's checkpoint. There's no retry queue: after any failure, the collector reads again from Docker's log files.

| Event | Result |
|---|---|
| Collector restart, database or proxy outage | Resumes from the checkpoints; nothing lost, no duplicates |
| Container removed before it was read to the end | Recorded as a `container_removed` gap |
| Collector down for over a minute | Recorded as a `collector_down` gap |
| Docker log damaged, for example by a power cut | Docker silently skips what's after the damage, so Blackbox checks for that, records a `log_damaged` gap, and keeps collecting new lines |

`status` and `gaps` show all of it.

## The CLI

```bash
docker exec -it blackbox-api /blackbox shell          # the prompt: blackbox (prod)>
docker exec blackbox-api /blackbox search --level error --since 6h
```

| Command | Shows |
|---|---|
| `search [text] [flags]` | Lines by time, project, service, container, version, level, stream, text or field |
| `show <id>` | One line: every field |
| `context <id>` | The lines around a line, for reading a stack trace |
| `status` | Collection health. Exits non-zero when something needs attention |
| `sources` | Every project and service with logs |
| `gaps` | Known and possible loss |
| `consumers`, `migrate status`, `version`, `help`, `exit` | Setup |

`help <command>` shows each one's flags and examples.

To leave a container out of collection, label it `com.mobasity.logs=off`.

## Running it locally

Needs Go 1.27.3 and Docker Desktop.

One-time setup, in the repo folder:

1. `docker network create spark`, if it doesn't exist yet.
2. On `sparkdb-dev` (`localhost:5000`), run `scripts/db/setup.sql` as the admin user, with dev passwords.
3. Copy `.env.example` to `.env` (gitignored) and fill in the dev values: the three database URLs pointing at `host.docker.internal:5000`, and a dev API token.

`docker-compose.dev.yml` publishes the API on `127.0.0.1:3191`. All three containers turn `healthy` in `docker ps`, and `docker exec blackbox-api /blackbox status` prints no "Needs attention" line.

```bash
go test ./...                                              # the tests
docker build --target test .                               # as Lighthouse runs them, with -race
scripts/test-db.sh up && eval "$(scripts/test-db.sh env)"  # a throwaway Postgres: the database tests run too
scripts/test-db.sh down                                    # remove it
docker compose -f docker-compose.yml -f docker-compose.dev.yml up --build
```

In Git Bash on Windows, set `MSYS_NO_PATHCONV=1` first, or `docker exec … /blackbox` has its path rewritten.

## Deploying

Lighthouse deploys it on version tags. It needs Lighthouse's `policy.json` to allow the Docker socket for `blackbox-proxy`, the `blackbox_db` database with its roles (`scripts/db/setup.sql`), and four keys in Cove: `BLACKBOX_DATABASE_URL`, `BLACKBOX_DATABASE_MIGRATOR_URL`, `BLACKBOX_DATABASE_READER_URL` and `BLACKBOX_CLI_TOKEN`.

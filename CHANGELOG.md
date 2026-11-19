# Changelog

All notable changes to Blackbox. Versions follow [semantic versioning](https://semver.org).

## v0.0.2 (2026-11-18)

Error counts and repeated errors, for the CLI and for Vista, Spark's dashboard, which becomes the API's second consumer.

### Added
- **Vista can use the API**, with its own token in `SHARED_BLACKBOX_VISTA_TOKEN` and the same read access as the CLI.
- **`GET /v1/stats/errors`** counts error and warning lines per project, per hour or day, with each project's totals.
- **`GET /v1/stats/errors/groups`** groups repeated lines by project, service, level and fingerprint: the message and its `err` field, with ids and numbers taken out. Each group has a count, when it was first and last seen, and its newest line, for `context`. Both endpoints cover the last 24 hours unless given a range, and at most 31 days.
- **The CLI's `errors` command** lists the repeated errors, most frequent first; `--level warn` adds warnings.
- An MIT `LICENSE`.

### Changed
- **`blackbox-api` needs a token for every consumer to start**, now including `SHARED_BLACKBOX_VISTA_TOKEN`.
- The image runs as user 10001; only `blackbox-proxy` runs as root, set in the compose file, to read the Docker socket.
- Blackbox's start line no longer repeats its version and environment, which are read from Docker.
- Go 1.27.4.
- The README is rewritten, with the features, the API and every setting.

### Fixed
- **A service that stops on an error logs why at `ERROR`**, instead of as plain text with no level, so searches and error counts include it.
- A shutdown that runs past its time limit is logged instead of ignored.

## v0.0.1 (2026-11-02)

Small fixes from Blackbox's first day collecting real logs.

### Changed
- **The API moved to port 3190** (`blackbox-api:3190` on `spark`), and to 3191 for local development. It stays unpublished in prod.

### Fixed
- **Ordinary sentences about cookies are no longer hidden:** chat lines such as "Your daily cookie: …" were redacted as if they were `Cookie:` headers. Real cookie headers and `cookie=` values still are.
- **Tabs in log lines show as spaces in the CLI**, not `?`. Multi-line Postgres statements are readable now.

## v0.0.0 (2026-11-02)

First version: every container's logs, collected, kept for 30 days and searchable from the CLI.

### Added

- `blackbox-proxy`: the only part with the Docker socket. It has no network, lists containers without their environment or mounts, and passes log bytes through without reading them.
- `blackbox-collector`: follows every container, running or stopped, unless it's labelled `com.mobasity.logs=off`. Each line's project, service and version come from Docker's labels.
- Lines are parsed from JSON, `slog` text, Go's `log`, cloudflared and Postgres. Anything else is kept as plain text with level `unknown`.
- Secrets in lines are hidden: bearer tokens, passwords in URLs, `Authorization` and `Cookie` values, and values under sensitive names.
- Each batch of lines is saved in one transaction with its containers' checkpoints, so a restart or an outage loses nothing and repeats nothing.
- Known and possible loss is recorded as a gap: a container removed before it was read, the collector being down, or a Docker log damaged by a power cut.
- Retention of 30 days, with a 1 GB cap that removes the oldest records first.
- `blackbox-api`: authenticated search, single records, surrounding lines, sources, gaps and status, on `blackbox-api:2600`.
- The CLI: `search`, `show`, `context`, `status`, `sources`, `gaps`, `consumers`, `migrate status`, `version`, `help`.
- Blackbox's own logs follow Spark's logging standard, through `blackboxlog`.

# Changelog

All notable changes to Blackbox. Versions follow [semantic versioning](https://semver.org).

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

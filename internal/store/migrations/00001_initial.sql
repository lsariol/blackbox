-- Blackbox's log store. Every time is UTC. Levels are small integers: 0 unknown,
-- 10 debug, 20 info, 30 warn, 40 error. Streams: 1 stdout, 2 stderr.

-- +goose Up

-- Container instances: where each line came from, and how far it's been read.
-- checkpoint_ns is a bigint because Docker's timestamps have nanoseconds and
-- timestamptz stops at microseconds: a rounded checkpoint would reread lines.
CREATE TABLE blackbox.containers (
    id             integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    docker_id      text NOT NULL UNIQUE,
    project        text NOT NULL,
    service        text NOT NULL,
    name           text NOT NULL,
    image          text NOT NULL,
    version        text NOT NULL,
    created_at     timestamptz,
    started_at     timestamptz,
    first_seen     timestamptz NOT NULL,
    last_seen      timestamptz NOT NULL,
    removed_at     timestamptz,
    checkpoint_ns  bigint NOT NULL,
    drained_at     timestamptz,
    damaged_at     timestamptz,
    last_record_at timestamptz,
    stream_state   text NOT NULL DEFAULT '',
    last_error     text NOT NULL DEFAULT ''
);

-- One row per log line. Project, service and version are on the container: a
-- new version means a new container, so each line's version is exact.
CREATE TABLE blackbox.records (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    container_id integer NOT NULL REFERENCES blackbox.containers (id),
    ts           timestamptz NOT NULL,
    stream       smallint NOT NULL,
    level        smallint NOT NULL,
    format       smallint NOT NULL,
    message      text NOT NULL,
    attrs        jsonb,
    truncated    boolean NOT NULL DEFAULT false,
    redacted     boolean NOT NULL DEFAULT false
);
CREATE INDEX records_ts ON blackbox.records (ts);
CREATE INDEX records_container_ts ON blackbox.records (container_id, ts, id);
CREATE INDEX records_serious ON blackbox.records (level, ts) WHERE level >= 30;

-- Known or possible loss.
CREATE TABLE blackbox.gaps (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind         text NOT NULL,
    container_id integer REFERENCES blackbox.containers (id) ON DELETE SET NULL,
    from_ts      timestamptz NOT NULL,
    to_ts        timestamptz NOT NULL,
    detail       text NOT NULL,
    noted_at     timestamptz NOT NULL
);
CREATE INDEX gaps_noted_at ON blackbox.gaps (noted_at);

-- What the collector last reported; the API has no other view of it.
CREATE TABLE blackbox.collector_status (
    id                boolean PRIMARY KEY DEFAULT true CHECK (id),
    heartbeat_at      timestamptz NOT NULL,
    version           text NOT NULL,
    last_read_at      timestamptz,
    last_persisted_at timestamptz,
    streams_open      integer NOT NULL DEFAULT 0,
    streams_refused   integer NOT NULL DEFAULT 0,
    lag_seconds       real NOT NULL DEFAULT 0,
    stored_bytes      bigint NOT NULL DEFAULT 0,
    last_error        text NOT NULL DEFAULT ''
);

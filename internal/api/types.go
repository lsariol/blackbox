package api

import (
	"encoding/json"
	"time"
)

// Record is one log line. Message and Attrs are as stored: untrusted text.
type Record struct {
	ID        int64           `json:"id"`
	Time      time.Time       `json:"ts"`
	Project   *string         `json:"project"` // null without compose labels
	Service   string          `json:"service"`
	Container string          `json:"container"`
	Image     string          `json:"image"`
	Version   *string         `json:"version"`
	Stream    string          `json:"stream"`
	Level     string          `json:"level"`
	Message   string          `json:"message"`
	Attrs     json.RawMessage `json:"attrs"`
	Truncated bool            `json:"truncated"`
	Redacted  bool            `json:"redacted"`
}

// LogsAnswer is one page of a search.
type LogsAnswer struct {
	Records        []Record `json:"records"`
	NextCursor     *string  `json:"next_cursor"`
	UnknownLeftOut int64    `json:"unknown_left_out"`
}

// ContextAnswer is the lines around a record, in order.
type ContextAnswer struct {
	Target  int64    `json:"target"`
	Records []Record `json:"records"`
}

// Source is one project and service with what's stored for it.
type Source struct {
	Project     *string    `json:"project"`
	Service     string     `json:"service"`
	Versions    []string   `json:"versions"`
	Newest      *time.Time `json:"newest"`
	Lines       int64      `json:"lines"`
	OpenStreams int        `json:"open_streams"`
}

// SourcesAnswer lists every source.
type SourcesAnswer struct {
	Sources []Source `json:"sources"`
}

// Gap is a stretch of known or possible loss.
type Gap struct {
	Kind      string    `json:"kind"`
	Container *string   `json:"container"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Detail    string    `json:"detail"`
	NotedAt   time.Time `json:"noted_at"`
}

// GapsAnswer lists gaps, newest first.
type GapsAnswer struct {
	Gaps []Gap `json:"gaps"`
}

// StatusAnswer is collection health. Attention is empty when all is well.
type StatusAnswer struct {
	GeneratedAt    time.Time  `json:"generated_at"`
	Version        *string    `json:"version"`
	HeartbeatAt    *time.Time `json:"heartbeat_at"`
	LastReadAt     *time.Time `json:"last_read_at"`
	LastSavedAt    *time.Time `json:"last_saved_at"`
	StreamsOpen    int        `json:"streams_open"`
	StreamsRefused int        `json:"streams_refused"`
	StreamsRead    int        `json:"streams_read"` // stopped containers read to the end
	LagSeconds     float64    `json:"lag_seconds"`
	StoredBytes    int64      `json:"stored_bytes"`
	MaxBytes       int64      `json:"max_bytes"`
	Oldest         *time.Time `json:"oldest"`
	Redacted24h    int64      `json:"redacted_24h"` // lines with something hidden
	Gaps24h        int64      `json:"gaps_24h"`
	Attention      []string   `json:"attention"`
}

// Migration is one of this build's migrations.
type Migration struct {
	Version   int64      `json:"version"`
	Name      string     `json:"name"`
	AppliedAt *time.Time `json:"applied_at"` // null while pending
}

// MigrationsAnswer lists the migrations, oldest first.
type MigrationsAnswer struct {
	Migrations []Migration `json:"migrations"`
}

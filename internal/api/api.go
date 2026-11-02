// Package api is blackbox-api: it serves log searches and collection status to
// authenticated consumers on the spark network. Answers are built field by
// field, and log text is returned as stored: anything showing it must treat it
// as untrusted.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lsariol/blackbox/internal/lines"
	"github.com/lsariol/blackbox/internal/store"
)

const (
	// Port is where the API listens, inside its container on spark.
	Port = 2600

	queryTimeout   = 5 * time.Second
	requestTimeout = 7 * time.Second
	connectRetry   = 5 * time.Second

	defaultLimit   = 100
	maxLimit       = 1000
	defaultContext = 20
	maxContext     = 200
	maxFields      = 5
	maxText        = 256
	defaultRange   = time.Hour

	// staleAfter is how old the collector's report may be.
	staleAfter = 30 * time.Second

	// storageWarnAt is the share of the storage cap that needs attention.
	storageWarnAt = 0.8

	// lagWarnAt is the lag, in seconds, that needs attention.
	lagWarnAt = 60
)

// Reader is the database, as the API reads it (blackbox_reader).
type Reader interface {
	Search(ctx context.Context, q store.Query) (store.Result, error)
	Record(ctx context.Context, id int64) (store.Row, error)
	Context(ctx context.Context, id int64, before, after int) ([]store.Row, error)
	Sources(ctx context.Context) ([]store.Source, error)
	Gaps(ctx context.Context, from, to time.Time) ([]store.GapRow, error)
	ReadStatus(ctx context.Context, now time.Time) (store.Status, error)
	Migrations(ctx context.Context) ([]store.Migration, error)
	CheckSchemaVersion(ctx context.Context) error
	Ping(ctx context.Context) error
}

// Options are the server's parts; tests swap them for fakes.
type Options struct {
	Version string
	Getenv  func(string) string // where tokens are read from
	Log     *slog.Logger
	Now     func() time.Time // time.Now unless set

	// OpenReader connects to the database; the server retries until it works.
	OpenReader func(ctx context.Context) (Reader, error)
}

// Server answers the API.
type Server struct {
	opts  Options
	creds []credential

	mu    sync.Mutex
	db    Reader
	dbErr error

	refused refusals
}

// New returns a server, or an error when a consumer's token is missing.
func New(opts Options) (*Server, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	creds, err := loadCredentials(opts.Getenv, consumers)
	if err != nil {
		return nil, err
	}
	return &Server{opts: opts, creds: creds, dbErr: errors.New("not connected yet")}, nil
}

// Handler routes the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /v1/logs", s.require(logsRead, s.logs))
	mux.HandleFunc("GET /v1/logs/{id}", s.require(logsRead, s.record))
	mux.HandleFunc("GET /v1/logs/{id}/context", s.require(logsRead, s.context))
	mux.HandleFunc("GET /v1/sources", s.require(logsRead, s.sources))
	mux.HandleFunc("GET /v1/gaps", s.require(logsRead, s.gaps))
	mux.HandleFunc("GET /v1/status", s.require(logsRead, s.status))
	mux.HandleFunc("GET /v1/migrations", s.require(logsRead, s.migrations))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint: the API serves /healthz and /v1/logs, /v1/sources, /v1/gaps, /v1/status, /v1/migrations")
	})
	return http.TimeoutHandler(securityHeaders(mux), requestTimeout, `{"error":"the request took too long"}`)
}

// Serve listens on addr until ctx is cancelled, connecting to the database in
// the background.
func (s *Server) Serve(ctx context.Context, addr string) error {
	go s.connect(ctx)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("api: listening on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	s.opts.Log.Info("API listening", "addr", addr, "consumers", len(s.creds))
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("api: %w", err)
	}
	return nil
}

// connect opens the database, retrying until it answers with the schema this
// build needs (the collector applies migrations).
func (s *Server) connect(ctx context.Context) {
	for {
		db, err := s.opts.OpenReader(ctx)
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, queryTimeout)
			err = db.CheckSchemaVersion(cctx)
			cancel()
		}
		s.mu.Lock()
		if err == nil {
			s.db, s.dbErr = db, nil
		} else if s.dbErr == nil || s.dbErr.Error() != err.Error() {
			s.opts.Log.Warn("waiting for the database", "err", err)
			s.dbErr = err
		} else {
			s.dbErr = err
		}
		s.mu.Unlock()
		if err == nil {
			s.opts.Log.Info("database ready")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(connectRetry):
		}
	}
}

func (s *Server) reader() (Reader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, s.dbErr
	}
	return s.db, nil
}

// healthz is unauthenticated, so it says only whether the API works.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	db, err := s.reader()
	if err == nil {
		err = db.Ping(r.Context())
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "version": s.opts.Version})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.opts.Version})
}

// require lets a request through only with a token whose consumer has scope.
func (s *Server) require(sc scope, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="blackbox"`)
			writeError(w, http.StatusUnauthorized, "a token is required: Authorization: Bearer <token>")
			return
		}
		c, ok := identify(s.creds, token)
		if !ok {
			s.refused.note(s.opts.Log, s.opts.Now(), "an unknown token", r)
			w.Header().Set("WWW-Authenticate", `Bearer realm="blackbox", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "unknown token")
			return
		}
		if !c.may(sc) {
			s.refused.note(s.opts.Log, s.opts.Now(), fmt.Sprintf("%s's token, which doesn't have %s", c.Name, sc), r)
			writeError(w, http.StatusForbidden, fmt.Sprintf("this token doesn't have %s", sc))
			return
		}
		next(w, r)
	}
}

// refusals logs refused requests: the first at once, then at most one summary
// a minute, so a flood of bad requests can't flood the log (and the disk).
type refusals struct {
	mu      sync.Mutex
	logged  time.Time
	pending int
}

func (f *refusals) note(log *slog.Logger, now time.Time, why string, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending++
	if now.Sub(f.logged) < time.Minute {
		return
	}
	log.Warn("refused requests", "count", f.pending, "latest", why, "path", r.URL.Path, "from", r.RemoteAddr)
	f.logged, f.pending = now, 0
}

func bearer(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

// withDB runs fn with the database and a query deadline, answering 503 when
// the database can't.
func (s *Server) withDB(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, db Reader) (any, error)) {
	db, err := s.reader()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "the database isn't ready yet; try again shortly")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	answer, err := fn(ctx, db)
	switch {
	case errors.Is(err, store.ErrNoRecord):
		writeError(w, http.StatusNotFound, "no such record: it may have aged out")
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		s.opts.Log.Warn("a query took too long", "path", r.URL.Path)
		writeError(w, http.StatusServiceUnavailable, "the search took too long: use a shorter time range or more filters")
	case err != nil:
		s.opts.Log.Error("building an answer failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusServiceUnavailable, "the database didn't answer; try again shortly")
	default:
		writeJSON(w, http.StatusOK, answer)
	}
}

// logs is the search.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	q, reason := parseQuery(r.URL.Query(), s.opts.Now())
	if reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return
	}
	s.withDB(w, r, func(ctx context.Context, db Reader) (any, error) {
		res, err := db.Search(ctx, q)
		if err != nil {
			return nil, err
		}
		answer := LogsAnswer{Records: toRecords(res.Rows), UnknownLeftOut: res.UnknownLeftOut}
		if res.More {
			last := res.Rows[len(res.Rows)-1]
			cursor := encodeCursor(store.Cursor{Time: last.Time, ID: last.ID})
			answer.NextCursor = &cursor
		}
		return answer, nil
	})
}

// parseQuery reads a search's parameters; reason says what's wrong with them.
func parseQuery(v url.Values, now time.Time) (q store.Query, reason string) {
	known := map[string]bool{"from": true, "to": true, "project": true, "service": true, "container": true,
		"version": true, "level": true, "stream": true, "q": true, "field": true, "order": true, "limit": true, "cursor": true}
	for key := range v {
		if !known[key] {
			return q, fmt.Sprintf("unknown parameter %q", key)
		}
	}

	q = store.Query{To: now, Limit: defaultLimit,
		Projects: v["project"], Services: v["service"], Containers: v["container"], Version: v.Get("version"), Text: v.Get("q")}
	q.From = q.To.Add(-defaultRange)
	var err error
	if t := v.Get("to"); t != "" {
		if q.To, err = time.Parse(time.RFC3339Nano, t); err != nil {
			return q, "to must be an RFC 3339 time, e.g. 2026-11-02T14:00:00Z"
		}
		q.From = q.To.Add(-defaultRange)
	}
	if t := v.Get("from"); t != "" {
		if q.From, err = time.Parse(time.RFC3339Nano, t); err != nil {
			return q, "from must be an RFC 3339 time, e.g. 2026-11-02T14:00:00Z"
		}
	}
	if q.From.After(q.To) {
		return q, "from must be before to"
	}
	if name := v.Get("level"); name != "" {
		level, ok := lines.LevelFromName(name)
		if !ok {
			return q, "level must be debug, info, warn or error"
		}
		q.MinLevel = level
	}
	switch v.Get("stream") {
	case "":
	case "stdout":
		q.Stream = lines.Stdout
	case "stderr":
		q.Stream = lines.Stderr
	default:
		return q, "stream must be stdout or stderr"
	}
	if len(q.Text) > maxText {
		return q, fmt.Sprintf("q can be at most %d bytes", maxText)
	}
	if len(v["field"]) > maxFields {
		return q, fmt.Sprintf("field can be given at most %d times", maxFields)
	}
	for _, f := range v["field"] {
		key, value, ok := strings.Cut(f, "=")
		if !ok || key == "" {
			return q, "field must be key=value"
		}
		q.Fields = append(q.Fields, store.Field{Key: key, Value: value})
	}
	switch v.Get("order") {
	case "", "newest":
	case "oldest":
		q.Oldest = true
	default:
		return q, "order must be newest or oldest"
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > maxLimit {
			return q, fmt.Sprintf("limit must be a number from 1 to %d", maxLimit)
		}
		q.Limit = n
	}
	if c := v.Get("cursor"); c != "" {
		cursor, err := decodeCursor(c)
		if err != nil {
			return q, "cursor isn't one this API gave out"
		}
		q.After = &cursor
	}
	return q, ""
}

// encodeCursor packs a page's end; the database keeps microseconds.
func encodeCursor(c store.Cursor) string {
	return base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "%d:%d", c.Time.UnixMicro(), c.ID))
}

func decodeCursor(s string) (store.Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return store.Cursor{}, err
	}
	at, id, ok := strings.Cut(string(raw), ":")
	if !ok {
		return store.Cursor{}, errors.New("no separator")
	}
	micros, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return store.Cursor{}, err
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return store.Cursor{}, err
	}
	return store.Cursor{Time: time.UnixMicro(micros).UTC(), ID: n}, nil
}

func parseID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// record is one record.
func (s *Server) record(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "id must be a record's number")
		return
	}
	s.withDB(w, r, func(ctx context.Context, db Reader) (any, error) {
		row, err := db.Record(ctx, id)
		if err != nil {
			return nil, err
		}
		return toRecord(row), nil
	})
}

// context is the lines around a record.
func (s *Server) context(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "id must be a record's number")
		return
	}
	before, ok1 := contextSize(r.URL.Query().Get("before"))
	after, ok2 := contextSize(r.URL.Query().Get("after"))
	if !ok1 || !ok2 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("before and after must be numbers from 0 to %d", maxContext))
		return
	}
	s.withDB(w, r, func(ctx context.Context, db Reader) (any, error) {
		rows, err := db.Context(ctx, id, before, after)
		if err != nil {
			return nil, err
		}
		return ContextAnswer{Target: id, Records: toRecords(rows)}, nil
	})
}

func contextSize(v string) (int, bool) {
	if v == "" {
		return defaultContext, true
	}
	n, err := strconv.Atoi(v)
	return n, err == nil && n >= 0 && n <= maxContext
}

// sources lists what's stored.
func (s *Server) sources(w http.ResponseWriter, r *http.Request) {
	s.withDB(w, r, func(ctx context.Context, db Reader) (any, error) {
		list, err := db.Sources(ctx)
		if err != nil {
			return nil, err
		}
		answer := SourcesAnswer{Sources: make([]Source, len(list))}
		for i, src := range list {
			versions := src.Versions
			if versions == nil {
				versions = []string{}
			}
			answer.Sources[i] = Source{Project: orNull(src.Project), Service: src.Service, Versions: versions,
				Newest: src.Newest, Lines: src.Lines, OpenStreams: src.OpenStreams}
		}
		return answer, nil
	})
}

// gaps lists known and possible loss.
func (s *Server) gaps(w http.ResponseWriter, r *http.Request) {
	now := s.opts.Now()
	to, from := now, now.Add(-7*24*time.Hour)
	var err error
	if v := r.URL.Query().Get("to"); v != "" {
		if to, err = time.Parse(time.RFC3339Nano, v); err != nil {
			writeError(w, http.StatusBadRequest, "to must be an RFC 3339 time")
			return
		}
	}
	if v := r.URL.Query().Get("from"); v != "" {
		if from, err = time.Parse(time.RFC3339Nano, v); err != nil {
			writeError(w, http.StatusBadRequest, "from must be an RFC 3339 time")
			return
		}
	}
	s.withDB(w, r, func(ctx context.Context, db Reader) (any, error) {
		list, err := db.Gaps(ctx, from, to)
		if err != nil {
			return nil, err
		}
		answer := GapsAnswer{Gaps: make([]Gap, len(list))}
		for i, g := range list {
			answer.Gaps[i] = Gap{Kind: g.Kind, Container: orNull(g.Container), From: g.From.UTC(), To: g.To.UTC(), Detail: g.Detail, NotedAt: g.NotedAt.UTC()}
		}
		return answer, nil
	})
}

// status is collection health.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	s.withDB(w, r, func(ctx context.Context, db Reader) (any, error) {
		now := s.opts.Now()
		st, err := db.ReadStatus(ctx, now)
		if err != nil {
			return nil, err
		}
		return buildStatus(st, now), nil
	})
}

// buildStatus shapes the status and decides what needs attention.
func buildStatus(st store.Status, now time.Time) StatusAnswer {
	answer := StatusAnswer{
		GeneratedAt: now.UTC(), Version: orNull(st.Version), HeartbeatAt: nil,
		LastReadAt: st.LastReadAt, LastSavedAt: st.LastPersistedAt,
		StreamsOpen: st.StreamsOpen, StreamsRefused: st.StreamsRefused, StreamsRead: st.Drained,
		LagSeconds: st.LagSeconds, StoredBytes: st.StoredBytes, MaxBytes: store.MaxBytes, Oldest: st.Oldest,
		Redacted24h: st.Redacted24h, Gaps24h: st.Gaps24h, Attention: []string{},
	}
	attention := func(format string, args ...any) {
		answer.Attention = append(answer.Attention, fmt.Sprintf(format, args...))
	}
	if !st.Reported {
		attention("the collector hasn't reported yet: check docker logs blackbox-collector")
		return answer
	}
	answer.HeartbeatAt = &st.HeartbeatAt
	if age := now.Sub(st.HeartbeatAt); age > staleAfter {
		attention("the collector last reported %s ago: check docker logs blackbox-collector", age.Round(time.Second))
	}
	if st.LastError != "" {
		attention("collector: %s", st.LastError)
	}
	if st.StreamsRefused > 0 {
		attention("%d log streams refused: the proxy is at its limit", st.StreamsRefused)
	}
	if st.LagSeconds > lagWarnAt {
		attention("collection is %d s behind", int(st.LagSeconds))
	}
	if float64(st.StoredBytes) >= storageWarnAt*float64(store.MaxBytes) {
		attention("storage is at %d%% of its cap: the oldest records go first", st.StoredBytes*100/store.MaxBytes)
	}
	for _, p := range st.Problems {
		switch {
		case p.Damaged:
			attention("%q has a damaged log: only new lines are collected until it's replaced", p.Container)
		case p.LastError != "":
			attention("%q: %s", p.Container, p.LastError)
		default:
			attention("%q: stream %s", p.Container, p.State)
		}
	}
	return answer
}

// migrations lists this build's migrations.
func (s *Server) migrations(w http.ResponseWriter, r *http.Request) {
	s.withDB(w, r, func(ctx context.Context, db Reader) (any, error) {
		list, err := db.Migrations(ctx)
		if err != nil {
			return nil, err
		}
		answer := MigrationsAnswer{Migrations: make([]Migration, len(list))}
		for i, m := range list {
			answer.Migrations[i] = Migration{Version: m.Version, Name: m.Name, AppliedAt: m.AppliedAt}
		}
		return answer, nil
	})
}

func toRecords(rows []store.Row) []Record {
	out := make([]Record, len(rows))
	for i, r := range rows {
		out[i] = toRecord(r)
	}
	return out
}

// toRecord copies a row's fields, one by one.
func toRecord(r store.Row) Record {
	return Record{
		ID: r.ID, Time: r.Time, Project: orNull(r.Project), Service: r.Service, Container: r.Container,
		Image: r.Image, Version: orNull(r.Version), Stream: lines.StreamName(r.Stream), Level: lines.LevelName(r.Level),
		Message: r.Message, Attrs: json.RawMessage(r.Attrs), Truncated: r.Truncated, Redacted: r.Redacted,
	}
}

func orNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Cursor marks where a page of records ended.
type Cursor struct {
	Time time.Time
	ID   int64
}

// Field is a key=value match on a record's attrs.
type Field struct {
	Key, Value string
}

// Query is a search of the records. Empty filters match everything.
type Query struct {
	From, To   time.Time
	Projects   []string
	Services   []string
	Containers []string
	Version    string
	MinLevel   int16 // 0: every level, unknown included
	Stream     int16 // 0: both
	Text       string
	Fields     []Field
	Oldest     bool
	Limit      int
	After      *Cursor
}

// Row is a record with its container's details.
type Row struct {
	ID        int64
	Time      time.Time
	Project   string
	Service   string
	Container string
	Image     string
	Version   string
	Stream    int16
	Level     int16
	Format    int16
	Message   string
	Attrs     []byte
	Truncated bool
	Redacted  bool
}

// Result is one page of a search.
type Result struct {
	Rows           []Row
	More           bool  // more rows match after this page
	UnknownLeftOut int64 // lines with no level that MinLevel left out, on the first page
}

const rowColumns = `r.id, r.ts, c.project, c.service, c.name, c.image, c.version,
	r.stream, r.level, r.format, r.message, r.attrs, r.truncated, r.redacted`

func scanRow(row pgx.CollectableRow) (Row, error) {
	var r Row
	err := row.Scan(&r.ID, &r.Time, &r.Project, &r.Service, &r.Container, &r.Image, &r.Version,
		&r.Stream, &r.Level, &r.Format, &r.Message, &r.Attrs, &r.Truncated, &r.Redacted)
	r.Time = r.Time.UTC()
	return r, err
}

// where builds the filters every search shares, leaving out the level.
type where struct {
	clauses []string
	args    []any
}

func (w *where) add(clause string, args ...any) {
	for _, a := range args {
		w.args = append(w.args, a)
		clause = strings.Replace(clause, "?", "$"+strconv.Itoa(len(w.args)), 1)
	}
	w.clauses = append(w.clauses, clause)
}

func (w *where) sql() string {
	return strings.Join(w.clauses, " AND ")
}

// likeEscaper escapes what ILIKE treats as wildcards.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func filters(q Query) *where {
	w := &where{}
	w.add("r.ts >= ?", q.From.UTC())
	w.add("r.ts <= ?", q.To.UTC())
	if len(q.Projects) > 0 {
		w.add("c.project = ANY(?)", q.Projects)
	}
	if len(q.Services) > 0 {
		w.add("c.service = ANY(?)", q.Services)
	}
	if len(q.Containers) > 0 {
		w.add("c.name = ANY(?)", q.Containers)
	}
	if q.Version != "" {
		w.add("c.version = ?", q.Version)
	}
	if q.Stream != 0 {
		w.add("r.stream = ?", q.Stream)
	}
	if q.Text != "" {
		pattern := "%" + likeEscaper.Replace(q.Text) + "%"
		w.args = append(w.args, pattern)
		n := "$" + strconv.Itoa(len(w.args))
		w.clauses = append(w.clauses, "(r.message ILIKE "+n+" OR r.attrs::text ILIKE "+n+")")
	}
	for _, f := range q.Fields {
		w.add("r.attrs ->> ? = ?", f.Key, f.Value)
	}
	return w
}

// Search returns one page of matching records, sorted by time.
func (s *Store) Search(ctx context.Context, q Query) (Result, error) {
	w := filters(q)
	unknown := q.MinLevel > 0 && q.After == nil
	var countSQL string
	var countArgs []any
	if unknown {
		countSQL = "SELECT count(*) FROM blackbox.records r JOIN blackbox.containers c ON c.id = r.container_id WHERE " + w.sql() + " AND r.level = 0"
		countArgs = append(countArgs, w.args...)
	}

	if q.MinLevel > 0 {
		w.add("r.level >= ?", q.MinLevel)
	}
	order, compare := "DESC", "<"
	if q.Oldest {
		order, compare = "ASC", ">"
	}
	if q.After != nil {
		w.args = append(w.args, q.After.Time, q.After.ID)
		n := len(w.args)
		w.clauses = append(w.clauses, fmt.Sprintf("(r.ts, r.id) %s ($%d, $%d)", compare, n-1, n))
	}
	w.args = append(w.args, q.Limit+1)
	sql := "SELECT " + rowColumns + ` FROM blackbox.records r
		JOIN blackbox.containers c ON c.id = r.container_id
		WHERE ` + w.sql() + " ORDER BY r.ts " + order + ", r.id " + order + " LIMIT $" + strconv.Itoa(len(w.args))

	rows, err := s.pool.Query(ctx, sql, w.args...)
	if err != nil {
		return Result{}, fmt.Errorf("database: searching: %w", err)
	}
	list, err := pgx.CollectRows(rows, scanRow)
	if err != nil {
		return Result{}, fmt.Errorf("database: searching: %w", err)
	}
	res := Result{Rows: list}
	if len(list) > q.Limit {
		res.Rows, res.More = list[:q.Limit], true
	}
	if unknown {
		if err := s.pool.QueryRow(ctx, countSQL, countArgs...).Scan(&res.UnknownLeftOut); err != nil {
			return Result{}, fmt.Errorf("database: counting lines with no level: %w", err)
		}
	}
	return res, nil
}

// ErrNoRecord means no record has that ID, or it has aged out.
var ErrNoRecord = errors.New("no such record")

// Record returns one record.
func (s *Store) Record(ctx context.Context, id int64) (Row, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+rowColumns+` FROM blackbox.records r
		JOIN blackbox.containers c ON c.id = r.container_id WHERE r.id = $1`, id)
	if err != nil {
		return Row{}, fmt.Errorf("database: reading a record: %w", err)
	}
	row, err := pgx.CollectExactlyOneRow(rows, scanRow)
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, ErrNoRecord
	}
	if err != nil {
		return Row{}, fmt.Errorf("database: reading a record: %w", err)
	}
	return row, nil
}

// Context returns the lines around a record in the same container, in order,
// with the record itself included.
func (s *Store) Context(ctx context.Context, id int64, before, after int) ([]Row, error) {
	rows, err := s.pool.Query(ctx, `
		WITH target AS (SELECT container_id, ts, id FROM blackbox.records WHERE id = $1),
		around AS (
			(SELECT r.* FROM blackbox.records r, target t
			  WHERE r.container_id = t.container_id AND (r.ts, r.id) < (t.ts, t.id)
			  ORDER BY r.ts DESC, r.id DESC LIMIT $2)
			UNION ALL
			(SELECT r.* FROM blackbox.records r, target t
			  WHERE r.container_id = t.container_id AND (r.ts, r.id) >= (t.ts, t.id)
			  ORDER BY r.ts, r.id LIMIT $3)
		)
		SELECT `+rowColumns+` FROM around r
		  JOIN blackbox.containers c ON c.id = r.container_id
		 ORDER BY r.ts, r.id`, id, before, after+1)
	if err != nil {
		return nil, fmt.Errorf("database: reading a record's context: %w", err)
	}
	list, err := pgx.CollectRows(rows, scanRow)
	if err != nil {
		return nil, fmt.Errorf("database: reading a record's context: %w", err)
	}
	if len(list) == 0 {
		return nil, ErrNoRecord
	}
	return list, nil
}

// Source is one project and service with records or a container.
type Source struct {
	Project     string
	Service     string
	Versions    []string
	Newest      *time.Time
	Lines       int64
	OpenStreams int
}

// Sources lists every project and service, with what's stored for each.
func (s *Store) Sources(ctx context.Context) ([]Source, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.project, c.service,
		       array_remove(array_agg(DISTINCT NULLIF(c.version, '')), NULL),
		       MAX(n.newest), COALESCE(SUM(n.lines), 0)::bigint,
		       COUNT(*) FILTER (WHERE c.stream_state = 'open')
		  FROM blackbox.containers c
		  LEFT JOIN (SELECT container_id, MAX(ts) AS newest, COUNT(*) AS lines
		               FROM blackbox.records GROUP BY container_id) n ON n.container_id = c.id
		 GROUP BY c.project, c.service
		 ORDER BY c.project, c.service`)
	if err != nil {
		return nil, fmt.Errorf("database: listing sources: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Source, error) {
		var src Source
		err := row.Scan(&src.Project, &src.Service, &src.Versions, &src.Newest, &src.Lines, &src.OpenStreams)
		return src, err
	})
	if err != nil {
		return nil, fmt.Errorf("database: listing sources: %w", err)
	}
	return list, nil
}

// GapRow is a recorded gap with its container's name.
type GapRow struct {
	Kind      string
	Container string // "" when it isn't one container's
	From, To  time.Time
	Detail    string
	NotedAt   time.Time
}

// Gaps lists the gaps noted in a time range, newest first.
func (s *Store) Gaps(ctx context.Context, from, to time.Time) ([]GapRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.kind, COALESCE(c.name, ''), g.from_ts, g.to_ts, g.detail, g.noted_at
		  FROM blackbox.gaps g
		  LEFT JOIN blackbox.containers c ON c.id = g.container_id
		 WHERE g.noted_at >= $1 AND g.noted_at <= $2
		 ORDER BY g.noted_at DESC, g.id DESC`, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("database: listing gaps: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (GapRow, error) {
		var g GapRow
		err := row.Scan(&g.Kind, &g.Container, &g.From, &g.To, &g.Detail, &g.NotedAt)
		return g, err
	})
	if err != nil {
		return nil, fmt.Errorf("database: listing gaps: %w", err)
	}
	return list, nil
}

// StreamProblem is a container whose log isn't being read normally.
type StreamProblem struct {
	Container string
	State     string
	LastError string
}

// Status is everything `status` shows.
type Status struct {
	Reported        bool // false before the collector's first report
	HeartbeatAt     time.Time
	Version         string
	LastReadAt      *time.Time
	LastPersistedAt *time.Time
	StreamsOpen     int
	StreamsRefused  int
	Drained         int
	LagSeconds      float64
	StoredBytes     int64
	LastError       string
	Oldest          *time.Time
	Redacted24h     int64
	Gaps24h         int64
	Problems        []StreamProblem
}

// ReadStatus gathers the collector's report and the figures around it.
func (s *Store) ReadStatus(ctx context.Context, now time.Time) (Status, error) {
	var st Status
	err := s.pool.QueryRow(ctx, `
		SELECT heartbeat_at, version, last_read_at, last_persisted_at, streams_open,
		       streams_refused, lag_seconds, stored_bytes, last_error
		  FROM blackbox.collector_status`).Scan(&st.HeartbeatAt, &st.Version, &st.LastReadAt, &st.LastPersistedAt,
		&st.StreamsOpen, &st.StreamsRefused, &st.LagSeconds, &st.StoredBytes, &st.LastError)
	switch {
	case err == nil:
		st.Reported = true
	case !errors.Is(err, pgx.ErrNoRows):
		return Status{}, fmt.Errorf("database: reading the collector's status: %w", err)
	}

	day := now.Add(-24 * time.Hour)
	err = s.pool.QueryRow(ctx, `
		SELECT (SELECT MIN(ts) FROM blackbox.records),
		       (SELECT COUNT(*) FROM blackbox.records WHERE redacted AND ts >= $1),
		       (SELECT COUNT(*) FROM blackbox.gaps WHERE noted_at >= $1),
		       (SELECT COUNT(*) FROM blackbox.containers WHERE removed_at IS NULL AND drained_at IS NOT NULL)`, day,
	).Scan(&st.Oldest, &st.Redacted24h, &st.Gaps24h, &st.Drained)
	if err != nil {
		return Status{}, fmt.Errorf("database: reading the status figures: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT name, stream_state, last_error FROM blackbox.containers
		 WHERE removed_at IS NULL AND (last_error <> '' OR stream_state IN ('failed', 'refused') OR damaged_at IS NOT NULL)
		 ORDER BY name`)
	if err != nil {
		return Status{}, fmt.Errorf("database: reading the streams: %w", err)
	}
	st.Problems, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (StreamProblem, error) {
		var p StreamProblem
		err := row.Scan(&p.Container, &p.State, &p.LastError)
		return p, err
	})
	if err != nil {
		return Status{}, fmt.Errorf("database: reading the streams: %w", err)
	}
	return st, nil
}

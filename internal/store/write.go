package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lsariol/blackbox/internal/containers"
)

// Gap kinds.
const (
	GapContainerRemoved = "container_removed"
	GapCollectorDown    = "collector_down"
	GapLogDamaged       = "log_damaged"
)

const (
	// deleteBatch is how many rows one retention delete removes.
	deleteBatch = 10000
)

// Tracked is a container the collector is reading, as the database has it.
type Tracked struct {
	ID         int32
	DockerID   string
	Checkpoint int64 // Unix nanoseconds of the last line saved
	Drained    bool  // stopped, and read to the end
	Damaged    bool  // its log can't be read from a checkpoint
	StartedAt  *time.Time
}

// Record is one log line to save.
type Record struct {
	ContainerID int32
	Time        time.Time
	Stream      int16
	Level       int16
	Format      int16
	Message     string
	Attrs       []byte // JSON, nil without fields
	Truncated   bool
	Redacted    bool
}

// Batch is what one flush saves: the records, and how far each container got.
type Batch struct {
	Records     []Record
	Checkpoints map[int32]int64
}

// Gap is a stretch of known or possible loss.
type Gap struct {
	Kind        string
	ContainerID *int32 // nil when it isn't one container's
	From, To    time.Time
	Detail      string
	NotedAt     time.Time
}

// CollectorStatus is what the collector reports every few seconds.
type CollectorStatus struct {
	HeartbeatAt     time.Time
	Version         string
	LastReadAt      *time.Time
	LastPersistedAt *time.Time
	StreamsOpen     int
	StreamsRefused  int
	LagSeconds      float64
	LastError       string
}

// StreamReport is one container's reading state, for `status`.
type StreamReport struct {
	ID        int32
	State     string
	LastError string
}

// Tracked lists the containers not yet known to be removed.
func (s *Store) Tracked(ctx context.Context) ([]Tracked, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, docker_id, checkpoint_ns, drained_at IS NOT NULL, damaged_at IS NOT NULL, started_at
		  FROM blackbox.containers WHERE removed_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("database: reading the containers: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Tracked, error) {
		var t Tracked
		err := row.Scan(&t.ID, &t.DockerID, &t.Checkpoint, &t.Drained, &t.Damaged, &t.StartedAt)
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("database: reading the containers: %w", err)
	}
	return list, nil
}

// Register adds a container seen for the first time, reading from checkpoint.
func (s *Store) Register(ctx context.Context, c containers.Container, checkpoint int64, now time.Time) (Tracked, error) {
	t := Tracked{DockerID: c.ID, Checkpoint: checkpoint, StartedAt: c.StartedAt}
	created := &c.CreatedAt
	if c.CreatedAt.IsZero() {
		created = nil
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO blackbox.containers
		       (docker_id, project, service, name, image, version, created_at, started_at, first_seen, last_seen, checkpoint_ns)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9, $10)
		ON CONFLICT (docker_id) DO UPDATE SET removed_at = NULL, last_seen = $9
		RETURNING id, checkpoint_ns, drained_at IS NOT NULL, damaged_at IS NOT NULL`,
		c.ID, c.Project, c.Service, c.Name, c.Image, c.Version, created, c.StartedAt, now, checkpoint,
	).Scan(&t.ID, &t.Checkpoint, &t.Drained, &t.Damaged)
	if err != nil {
		return Tracked{}, fmt.Errorf("database: registering %q: %w", c.Name, err)
	}
	return t, nil
}

// Save stores a batch in one transaction, so a crash can't keep the records
// without the checkpoints or the other way round.
func (s *Store) Save(ctx context.Context, b Batch, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: saving records: %w", err)
	}
	defer tx.Rollback(context.Background())

	rows := make([][]any, len(b.Records))
	for i, r := range b.Records {
		var attrs any
		if r.Attrs != nil {
			attrs = string(r.Attrs)
		}
		rows[i] = []any{r.ContainerID, r.Time.UTC(), r.Stream, r.Level, r.Format, r.Message, attrs, r.Truncated, r.Redacted}
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"blackbox", "records"},
		[]string{"container_id", "ts", "stream", "level", "format", "message", "attrs", "truncated", "redacted"},
		pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("database: saving records: %w", err)
	}
	for id, checkpoint := range b.Checkpoints {
		_, err := tx.Exec(ctx, `
			UPDATE blackbox.containers
			   SET checkpoint_ns = GREATEST(checkpoint_ns, $2), last_record_at = $3
			 WHERE id = $1`, id, checkpoint, time.Unix(0, checkpoint).UTC())
		if err != nil {
			return fmt.Errorf("database: saving a checkpoint: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: saving records: %w", err)
	}
	return nil
}

// SetDrained marks a container as read to the end, or not.
func (s *Store) SetDrained(ctx context.Context, id int32, at *time.Time, started *time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE blackbox.containers SET drained_at = $2, started_at = $3 WHERE id = $1`, id, at, started)
	if err != nil {
		return fmt.Errorf("database: updating a container: %w", err)
	}
	return nil
}

// SetRemoved marks a container as gone from Docker.
func (s *Store) SetRemoved(ctx context.Context, id int32, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE blackbox.containers SET removed_at = $2, stream_state = 'removed' WHERE id = $1`, id, at)
	if err != nil {
		return fmt.Errorf("database: updating a container: %w", err)
	}
	return nil
}

// SetDamaged records that a container's log can't be read from a checkpoint,
// moves its checkpoint to now and notes the gap, all at once.
func (s *Store) SetDamaged(ctx context.Context, id int32, gap Gap) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: recording a damaged log: %w", err)
	}
	defer tx.Rollback(context.Background())

	_, err = tx.Exec(ctx, `
		UPDATE blackbox.containers
		   SET damaged_at = COALESCE(damaged_at, $2), checkpoint_ns = GREATEST(checkpoint_ns, $3)
		 WHERE id = $1`, id, gap.NotedAt, gap.To.UnixNano())
	if err != nil {
		return fmt.Errorf("database: recording a damaged log: %w", err)
	}
	if err := insertGap(ctx, tx, gap); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: recording a damaged log: %w", err)
	}
	return nil
}

// AddGap records known or possible loss.
func (s *Store) AddGap(ctx context.Context, gap Gap) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: recording a gap: %w", err)
	}
	defer tx.Rollback(context.Background())
	if err := insertGap(ctx, tx, gap); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: recording a gap: %w", err)
	}
	return nil
}

func insertGap(ctx context.Context, tx pgx.Tx, g Gap) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO blackbox.gaps (kind, container_id, from_ts, to_ts, detail, noted_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, g.Kind, g.ContainerID, g.From, g.To, g.Detail, g.NotedAt)
	if err != nil {
		return fmt.Errorf("database: recording a gap: %w", err)
	}
	return nil
}

// LastHeartbeat returns when the collector last reported; ok is false before
// its first run.
func (s *Store) LastHeartbeat(ctx context.Context) (at time.Time, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT heartbeat_at FROM blackbox.collector_status`).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("database: reading the collector's status: %w", err)
	}
	return at, true, nil
}

// Report saves the collector's status and each container's reading state.
func (s *Store) Report(ctx context.Context, st CollectorStatus, streams []StreamReport, seen []int32) error {
	batch := &pgx.Batch{}
	batch.Queue(`
		INSERT INTO blackbox.collector_status
		       (id, heartbeat_at, version, last_read_at, last_persisted_at, streams_open, streams_refused, lag_seconds, last_error)
		VALUES (true, $1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET
		       heartbeat_at = $1, version = $2, last_read_at = $3, last_persisted_at = $4,
		       streams_open = $5, streams_refused = $6, lag_seconds = $7, last_error = $8`,
		st.HeartbeatAt, st.Version, st.LastReadAt, st.LastPersistedAt, st.StreamsOpen, st.StreamsRefused, st.LagSeconds, st.LastError)
	for _, r := range streams {
		batch.Queue(`UPDATE blackbox.containers SET stream_state = $2, last_error = $3 WHERE id = $1`, r.ID, r.State, r.LastError)
	}
	if len(seen) > 0 {
		batch.Queue(`UPDATE blackbox.containers SET last_seen = $2 WHERE id = ANY($1)`, seen, st.HeartbeatAt)
	}
	if err := s.pool.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("database: saving the collector's status: %w", err)
	}
	return nil
}

// Retain deletes what's past retention, then the oldest records while they
// take more than MaxBytes, and saves how much is stored. The size is added up
// from the rows: a delete doesn't shrink the table's files, so measuring those
// would keep deleting until nothing was left.
func (s *Store) Retain(ctx context.Context, now time.Time) (deleted, stored int64, err error) {
	cutoff := now.Add(-Retention)
	for {
		tag, err := s.pool.Exec(ctx, `
			DELETE FROM blackbox.records WHERE id IN (
				SELECT id FROM blackbox.records WHERE ts < $1 ORDER BY ts LIMIT $2)`, cutoff, deleteBatch)
		if err != nil {
			return deleted, 0, fmt.Errorf("database: deleting old records: %w", err)
		}
		deleted += tag.RowsAffected()
		if tag.RowsAffected() < deleteBatch {
			break
		}
	}

	stored, err = s.storedBytes(ctx)
	if err != nil {
		return deleted, 0, err
	}
	if excess := stored - MaxBytes; excess > 0 {
		n, err := s.deleteOldest(ctx, excess)
		if err != nil {
			return deleted, 0, err
		}
		deleted += n
		if stored, err = s.storedBytes(ctx); err != nil {
			return deleted, 0, err
		}
	}

	if _, err := s.pool.Exec(ctx, `DELETE FROM blackbox.gaps WHERE noted_at < $1`, cutoff); err != nil {
		return deleted, 0, fmt.Errorf("database: deleting old gaps: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM blackbox.containers c
		 WHERE c.removed_at < $1
		   AND NOT EXISTS (SELECT 1 FROM blackbox.records r WHERE r.container_id = c.id)`, cutoff); err != nil {
		return deleted, 0, fmt.Errorf("database: deleting old containers: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE blackbox.collector_status SET stored_bytes = $1`, stored); err != nil {
		return deleted, 0, fmt.Errorf("database: saving the stored size: %w", err)
	}
	return deleted, stored, nil
}

// recordSize estimates a row: its text plus 100 bytes of header and index entries.
const recordSize = `pg_column_size(message) + COALESCE(pg_column_size(attrs), 0) + 100`

func (s *Store) storedBytes(ctx context.Context) (int64, error) {
	var stored int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(SUM(`+recordSize+`), 0)::bigint FROM blackbox.records`).Scan(&stored)
	if err != nil {
		return 0, fmt.Errorf("database: measuring the records: %w", err)
	}
	return stored, nil
}

// deleteOldest removes the oldest records adding up to at least excess bytes.
func (s *Store) deleteOldest(ctx context.Context, excess int64) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM blackbox.records WHERE id IN (
			SELECT id FROM (
				SELECT id, SUM(`+recordSize+`) OVER (ORDER BY ts, id) - (`+recordSize+`) AS before
				  FROM blackbox.records
			) sized WHERE before < $1)`, excess)
	if err != nil {
		return 0, fmt.Errorf("database: deleting the oldest records: %w", err)
	}
	return tag.RowsAffected(), nil
}

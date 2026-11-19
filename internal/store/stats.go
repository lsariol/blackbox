package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Bucket is how error counts are grouped in time, on UTC boundaries.
type Bucket string

const (
	BucketHour Bucket = "hour"
	BucketDay  Bucket = "day"
)

// ErrorCount is one project's warn and error lines in one bucket.
type ErrorCount struct {
	Start   time.Time // the bucket's start
	Project string
	Errors  int64
	Warns   int64
}

// ErrorCounts counts warn and error lines per project per bucket in [from, to], leaving out empty buckets.
func (s *Store) ErrorCounts(ctx context.Context, from, to time.Time, bucket Bucket, projects []string) ([]ErrorCount, error) {
	w := &where{}
	w.add("r.level >= ?", int16(30))
	w.add("r.ts >= ?", from.UTC())
	w.add("r.ts <= ?", to.UTC())
	if len(projects) > 0 {
		w.add("c.project = ANY(?)", projects)
	}
	w.args = append(w.args, string(bucket))
	rows, err := s.pool.Query(ctx, `
		SELECT date_trunc($`+strconv.Itoa(len(w.args))+`, r.ts, 'UTC') AS start, c.project,
		       COUNT(*) FILTER (WHERE r.level >= 40), COUNT(*) FILTER (WHERE r.level < 40)
		  FROM blackbox.records r
		  JOIN blackbox.containers c ON c.id = r.container_id
		 WHERE `+w.sql()+`
		 GROUP BY 1, 2
		 ORDER BY 1, 2`, w.args...)
	if err != nil {
		return nil, fmt.Errorf("database: counting errors: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ErrorCount, error) {
		var c ErrorCount
		err := row.Scan(&c.Start, &c.Project, &c.Errors, &c.Warns)
		c.Start = c.Start.UTC()
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("database: counting errors: %w", err)
	}
	return list, nil
}

// fingerprintSQL is the message and err field with UUIDs, mixed ids (not 1500ms) and numbers taken out.
const fingerprintSQL = `regexp_replace(regexp_replace(regexp_replace(
	left(r.message || COALESCE(' err=' || (r.attrs ->> 'err'), ''), 500),
	'[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}', '<id>', 'gi'),
	'\m(?![0-9]+[a-z]*\M)(?=\w*[0-9])(?=\w*[a-z])\w{7,}\M', '<id>', 'gi'),
	'[0-9]+(\.[0-9]+)*', '<n>', 'g')`

// ErrorGroup is the lines of one service and level that share a fingerprint.
type ErrorGroup struct {
	Fingerprint   string
	Project       string
	Service       string
	Level         int16
	Count         int64
	FirstSeen     time.Time
	LastSeen      time.Time
	NewestID      int64
	NewestMessage string
}

// GroupsQuery picks the lines ErrorGroups groups.
type GroupsQuery struct {
	From, To time.Time
	MinLevel int16 // 30 (warn and error) or 40 (error)
	Projects []string
	Limit    int
}

// ErrorGroups groups lines at MinLevel and up by source, level and fingerprint; total is before Limit.
func (s *Store) ErrorGroups(ctx context.Context, q GroupsQuery) (groups []ErrorGroup, total int64, err error) {
	w := &where{}
	w.add("r.level >= ?", q.MinLevel)
	w.add("r.ts >= ?", q.From.UTC())
	w.add("r.ts <= ?", q.To.UTC())
	if len(q.Projects) > 0 {
		w.add("c.project = ANY(?)", q.Projects)
	}
	w.args = append(w.args, q.Limit)
	rows, err := s.pool.Query(ctx, `
		WITH lines AS MATERIALIZED (
			SELECT r.id, r.ts, r.level, r.message, c.project, c.service, `+fingerprintSQL+` COLLATE "C" AS fp
			  FROM blackbox.records r
			  JOIN blackbox.containers c ON c.id = r.container_id
			 WHERE `+w.sql()+`
		),
		groups AS (
			SELECT project, service, level, fp, COUNT(*) AS n, MIN(ts) AS first_seen, MAX(ts) AS last_seen,
			       COUNT(*) OVER () AS total
			  FROM lines
			 GROUP BY project, service, level, fp
			 ORDER BY n DESC, last_seen DESC, project, service, fp
			 LIMIT $`+strconv.Itoa(len(w.args))+`
		),
		newest AS (
			-- Each group's lines at its last_seen: a hash join, not a sort of every line.
			SELECT DISTINCT ON (project, service, level, fp) project, service, level, fp, l.id, l.message
			  FROM lines l
			  JOIN groups g USING (project, service, level, fp)
			 WHERE l.ts = g.last_seen
			 ORDER BY project, service, level, fp, l.id DESC
		)
		SELECT g.fp, g.project, g.service, g.level, g.n, g.first_seen, g.last_seen, g.total, n.id, n.message
		  FROM groups g
		  JOIN newest n USING (project, service, level, fp)
		 ORDER BY g.n DESC, g.last_seen DESC, g.project, g.service, g.fp`, w.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("database: grouping errors: %w", err)
	}
	groups, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (ErrorGroup, error) {
		var g ErrorGroup
		err := row.Scan(&g.Fingerprint, &g.Project, &g.Service, &g.Level, &g.Count, &g.FirstSeen, &g.LastSeen,
			&total, &g.NewestID, &g.NewestMessage)
		g.FirstSeen, g.LastSeen = g.FirstSeen.UTC(), g.LastSeen.UTC()
		return g, err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("database: grouping errors: %w", err)
	}
	return groups, total, nil
}

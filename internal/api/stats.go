package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/lsariol/blackbox/internal/lines"
	"github.com/lsariol/blackbox/internal/store"
)

// errorCounts is warn and error lines per project per hour or day.
func (s *Server) errorCounts(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	if reason := onlyParams(v, "from", "to", "bucket", "project"); reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return
	}
	from, to, reason := statsRange(v, s.opts.Now())
	if reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return
	}
	bucket := store.BucketHour
	switch v.Get("bucket") {
	case "", "hour":
	case "day":
		bucket = store.BucketDay
	default:
		writeError(w, http.StatusBadRequest, "bucket must be hour or day")
		return
	}
	s.withDB(w, r, func(ctx context.Context, db Reader) (any, error) {
		counts, err := db.ErrorCounts(ctx, from, to, bucket, v["project"])
		if err != nil {
			return nil, err
		}
		return buildErrorCounts(counts, from, to, bucket), nil
	})
}

// buildErrorCounts shapes the counts and adds each project's totals, in
// project order.
func buildErrorCounts(counts []store.ErrorCount, from, to time.Time, bucket store.Bucket) ErrorCountsAnswer {
	answer := ErrorCountsAnswer{From: from.UTC(), To: to.UTC(), Bucket: string(bucket),
		Buckets: make([]ErrorBucket, len(counts)), Totals: []ErrorTotal{}}
	totals := map[string]*ErrorTotal{}
	var order []string
	for i, c := range counts {
		answer.Buckets[i] = ErrorBucket{Start: c.Start, Project: orNull(c.Project), Errors: c.Errors, Warns: c.Warns}
		t, ok := totals[c.Project]
		if !ok {
			t = &ErrorTotal{Project: orNull(c.Project)}
			totals[c.Project] = t
			order = append(order, c.Project)
		}
		t.Errors += c.Errors
		t.Warns += c.Warns
	}
	slices.Sort(order)
	for _, p := range order {
		answer.Totals = append(answer.Totals, *totals[p])
	}
	return answer
}

// errorGroups is repeated lines grouped by fingerprint.
func (s *Server) errorGroups(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	if reason := onlyParams(v, "from", "to", "level", "project", "limit"); reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return
	}
	from, to, reason := statsRange(v, s.opts.Now())
	if reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return
	}
	q := store.GroupsQuery{From: from, To: to, MinLevel: lines.LevelError, Projects: v["project"], Limit: defaultGroups}
	switch v.Get("level") {
	case "", "error":
	case "warn":
		q.MinLevel = lines.LevelWarn
	default:
		writeError(w, http.StatusBadRequest, "level must be warn or error")
		return
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > maxGroups {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be a number from 1 to %d", maxGroups))
			return
		}
		q.Limit = n
	}
	s.withDB(w, r, func(ctx context.Context, db Reader) (any, error) {
		groups, total, err := db.ErrorGroups(ctx, q)
		if err != nil {
			return nil, err
		}
		answer := ErrorGroupsAnswer{From: from.UTC(), To: to.UTC(), Level: lines.LevelName(q.MinLevel),
			Groups: make([]ErrorGroup, len(groups)), TotalGroups: total}
		for i, g := range groups {
			answer.Groups[i] = ErrorGroup{Fingerprint: g.Fingerprint, Project: orNull(g.Project), Service: g.Service,
				Level: lines.LevelName(g.Level), Count: g.Count, FirstSeen: g.FirstSeen, LastSeen: g.LastSeen,
				NewestID: g.NewestID, NewestMessage: g.NewestMessage}
		}
		return answer, nil
	})
}

// onlyParams refuses a parameter the endpoint doesn't know.
func onlyParams(v url.Values, known ...string) string {
	for key := range v {
		if !slices.Contains(known, key) {
			return fmt.Sprintf("unknown parameter %q", key)
		}
	}
	return ""
}

// statsRange reads from and to: the last day unless given, at most 31 days.
func statsRange(v url.Values, now time.Time) (from, to time.Time, reason string) {
	to = now
	var err error
	if t := v.Get("to"); t != "" {
		if to, err = time.Parse(time.RFC3339Nano, t); err != nil {
			return from, to, "to must be an RFC 3339 time, e.g. 2026-11-02T14:00:00Z"
		}
	}
	from = to.Add(-defaultStatsRange)
	if t := v.Get("from"); t != "" {
		if from, err = time.Parse(time.RFC3339Nano, t); err != nil {
			return from, to, "from must be an RFC 3339 time, e.g. 2026-11-02T14:00:00Z"
		}
	}
	switch {
	case from.After(to):
		return from, to, "from must be before to"
	case to.Sub(from) > maxStatsRange:
		return from, to, "the range can be at most 31 days: Blackbox keeps 30"
	}
	return from, to, ""
}

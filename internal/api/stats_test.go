package api

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/lsariol/blackbox/internal/lines"
	"github.com/lsariol/blackbox/internal/store"
)

func TestErrorCountsAnswer(t *testing.T) {
	hour := now.Truncate(time.Hour)
	f := &fakeReader{counts: []store.ErrorCount{
		{Start: hour.Add(-time.Hour), Project: "sonar", Errors: 1},
		{Start: hour, Project: "lys", Errors: 2, Warns: 3},
		{Start: hour, Project: "", Warns: 4},
		{Start: hour, Project: "sonar", Errors: 5, Warns: 1},
	}}
	code, body, _ := call(t, newServer(t, f), "/v1/stats/errors?bucket=day&project=lys", token)
	if code != http.StatusOK {
		t.Fatalf("%d: %s", code, body)
	}
	a := decode[ErrorCountsAnswer](t, body)
	if f.lastBucket != store.BucketDay || a.Bucket != "day" || !a.To.Equal(now) || !a.From.Equal(now.Add(-24*time.Hour)) {
		t.Errorf("bucket %q, answer %+v", f.lastBucket, a)
	}
	if len(a.Buckets) != 4 || a.Buckets[2].Project != nil || *a.Buckets[1].Project != "lys" {
		t.Errorf("buckets = %+v", a.Buckets)
	}
	// Totals are in project order, with no project first.
	if len(a.Totals) != 3 || a.Totals[0].Project != nil || a.Totals[0].Warns != 4 ||
		*a.Totals[1].Project != "lys" || *a.Totals[2].Project != "sonar" || a.Totals[2].Errors != 6 || a.Totals[2].Warns != 1 {
		t.Errorf("totals = %s", body)
	}
}

func TestErrorCountsWithNothingIsEmptyLists(t *testing.T) {
	_, body, _ := call(t, newServer(t, &fakeReader{}), "/v1/stats/errors", token)
	a := decode[map[string]any](t, body)
	if b, ok := a["buckets"].([]any); !ok || len(b) != 0 {
		t.Errorf("buckets: %s", body)
	}
	if b, ok := a["totals"].([]any); !ok || len(b) != 0 {
		t.Errorf("totals: %s", body)
	}
}

func TestErrorGroupsAnswer(t *testing.T) {
	f := &fakeReader{groups: []store.ErrorGroup{{Fingerprint: "query failed after <n>ms", Project: "lys", Service: "lys",
		Level: lines.LevelError, Count: 3, FirstSeen: now.Add(-time.Hour), LastSeen: now, NewestID: 42, NewestMessage: "query failed after 9ms"}}}
	code, body, _ := call(t, newServer(t, f), "/v1/stats/errors/groups?level=warn&limit=5&project=lys", token)
	if code != http.StatusOK {
		t.Fatalf("%d: %s", code, body)
	}
	if q := f.lastGroups; q.MinLevel != lines.LevelWarn || q.Limit != 5 || len(q.Projects) != 1 || !q.To.Equal(now) {
		t.Errorf("query = %+v", q)
	}
	a := decode[ErrorGroupsAnswer](t, body)
	g := a.Groups[0]
	if a.Level != "warn" || a.TotalGroups != 8 || g.Level != "error" || *g.Project != "lys" || g.Count != 3 ||
		g.NewestID != 42 || g.Fingerprint != "query failed after <n>ms" {
		t.Errorf("answer = %s", body)
	}

	call(t, newServer(t, f), "/v1/stats/errors/groups", token)
	if q := f.lastGroups; q.MinLevel != lines.LevelError || q.Limit != defaultGroups {
		t.Errorf("defaults = %+v", q)
	}
}

func TestBadStatsRequestsAre400(t *testing.T) {
	s := newServer(t, &fakeReader{})
	for _, path := range []string{
		"/v1/stats/errors?bucket=week",
		"/v1/stats/errors?level=error",
		"/v1/stats/errors?from=yesterday",
		"/v1/stats/errors?from=2026-11-02T12:00:00Z&to=2026-11-01T12:00:00Z",
		"/v1/stats/errors?from=2026-09-01T00:00:00Z&to=2026-11-02T00:00:00Z",
		"/v1/stats/errors/groups?level=info",
		"/v1/stats/errors/groups?limit=0",
		"/v1/stats/errors/groups?limit=501",
		"/v1/stats/errors/groups?bucket=hour",
	} {
		if code, body, _ := call(t, s, path, token); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", path, code, body)
		}
	}
}

func TestStatsRange(t *testing.T) {
	from, to, reason := statsRange(url.Values{"to": {"2026-11-02T00:00:00Z"}}, now)
	if reason != "" || !to.Equal(now.Add(-12*time.Hour)) || !from.Equal(to.Add(-24*time.Hour)) {
		t.Errorf("from %s, to %s, %q", from, to, reason)
	}
	_, _, reason = statsRange(url.Values{"from": {"2026-10-02T12:00:00Z"}}, now)
	if reason != "" {
		t.Errorf("31 days should be allowed: %q", reason)
	}
}

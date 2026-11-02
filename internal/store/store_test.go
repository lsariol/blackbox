package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsariol/blackbox/internal/containers"
)

// The integration tests run against a real, disposable Postgres set up with
// scripts/db/setup.sql (scripts/test-db.sh does it). Without these variables
// they're skipped.
var (
	migratorURL = os.Getenv("BLACKBOX_TEST_DATABASE_MIGRATOR_URL")
	appURL      = os.Getenv("BLACKBOX_TEST_DATABASE_URL")
	readerURL   = os.Getenv("BLACKBOX_TEST_DATABASE_READER_URL")
)

var migrateOnce sync.Once

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

var day = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// open migrates once, connects as the app role and empties the tables.
func open(t *testing.T) *Store {
	t.Helper()
	if migratorURL == "" || appURL == "" || readerURL == "" {
		t.Skip("no test database: run scripts/test-db.sh up, then eval \"$(scripts/test-db.sh env)\"")
	}
	ctx := context.Background()
	var migrateErr error
	migrateOnce.Do(func() { migrateErr = Migrate(ctx, migratorURL, quiet) })
	if migrateErr != nil {
		t.Fatal(migrateErr)
	}
	s, err := Connect(ctx, appURL, "BLACKBOX_TEST_DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for _, table := range []string{"gaps", "records", "containers", "collector_status"} {
		if _, err := s.pool.Exec(ctx, "DELETE FROM blackbox."+table); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func reader(t *testing.T) *Store {
	t.Helper()
	s, err := Connect(context.Background(), readerURL, "BLACKBOX_TEST_DATABASE_READER_URL")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// add registers a container.
func add(t *testing.T, s *Store, name, project, service, version string) Tracked {
	t.Helper()
	c := containers.Container{ID: strings.Repeat(name[:1], 64), Name: name, Project: project, Service: service,
		Image: "img", Version: version, CreatedAt: day}
	tracked, err := s.Register(context.Background(), c, day.UnixNano(), day)
	if err != nil {
		t.Fatal(err)
	}
	return tracked
}

// write saves one line per message, a second apart from at.
func write(t *testing.T, s *Store, tr Tracked, at time.Time, level int16, messages ...string) {
	t.Helper()
	b := Batch{Checkpoints: map[int32]int64{}}
	for i, m := range messages {
		ts := at.Add(time.Duration(i) * time.Second)
		b.Records = append(b.Records, Record{ContainerID: tr.ID, Time: ts, Stream: 2, Level: level, Message: m})
		b.Checkpoints[tr.ID] = ts.UnixNano()
	}
	if err := s.Save(context.Background(), b, at); err != nil {
		t.Fatal(err)
	}
}

func messages(rows []Row) string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Message)
	}
	return strings.Join(out, ",")
}

func wide() Query {
	return Query{From: day.Add(-time.Hour), To: day.Add(time.Hour), Limit: 100}
}

func TestMigrateAgainIsFine(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := Migrate(ctx, migratorURL, quiet); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckSchemaVersion(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := reader(t).Migrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) < 2 || list[0].AppliedAt == nil {
		t.Errorf("migrations = %+v", list)
	}
}

func TestRegisterKeepsProgress(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tr := add(t, s, "lys", "lys", "lys", "v1")
	write(t, s, tr, day, 20, "hello")

	again, err := s.Register(ctx, containers.Container{ID: tr.DockerID, Name: "lys", CreatedAt: day}, 1, day)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != tr.ID || again.Checkpoint != day.UnixNano() {
		t.Errorf("registering again changed it: %+v", again)
	}
}

func TestSaveKeepsNanosecondCheckpoints(t *testing.T) {
	s := open(t)
	tr := add(t, s, "lys", "lys", "lys", "v1")
	at := day.Add(123456789 * time.Nanosecond)
	write(t, s, tr, at, 20, "hello")

	list, err := s.Tracked(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Checkpoint != at.UnixNano() {
		t.Errorf("checkpoint = %+v, want %d", list, at.UnixNano())
	}
}

func TestSaveIsAllOrNothing(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tr := add(t, s, "lys", "lys", "lys", "v1")
	bad := Batch{
		Records:     []Record{{ContainerID: tr.ID, Time: day, Message: "fine"}, {ContainerID: tr.ID, Time: day, Message: "bad", Attrs: []byte(`{"a":`)}},
		Checkpoints: map[int32]int64{tr.ID: day.Add(time.Hour).UnixNano()},
	}
	err := s.Save(ctx, bad, day)
	if err == nil {
		t.Fatal("broken JSON should be refused")
	}
	if !IsBadData(err) {
		t.Errorf("IsBadData = false for %v", err)
	}
	list, _ := s.Tracked(ctx)
	if list[0].Checkpoint != day.UnixNano() {
		t.Error("the checkpoint moved without its records")
	}
	res, _ := s.Search(ctx, wide())
	if len(res.Rows) != 0 {
		t.Errorf("records were kept: %+v", res.Rows)
	}
}

func TestSearchFilters(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	lys := add(t, s, "lys", "lys", "bot", "v1")
	web := add(t, s, "web", "web", "site", "v2")
	write(t, s, lys, day, 40, "Twitch: connection refused")
	write(t, s, lys, day.Add(time.Second), 20, "lookup 100%_done")
	write(t, s, web, day.Add(2*time.Second), 30, "slow page")
	b := Batch{Records: []Record{{ContainerID: web.ID, Time: day.Add(3 * time.Second), Stream: 1, Message: "plain", Attrs: []byte(`{"request_id":"abc","count":5}`)}}}
	if err := s.Save(ctx, b, day); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		edit func(*Query)
		want string
	}{
		{"everything", func(q *Query) {}, "plain,slow page,lookup 100%_done,Twitch: connection refused"},
		{"oldest first", func(q *Query) { q.Oldest = true }, "Twitch: connection refused,lookup 100%_done,slow page,plain"},
		{"time range", func(q *Query) { q.From = day.Add(time.Second); q.To = day.Add(2 * time.Second) }, "slow page,lookup 100%_done"},
		{"project", func(q *Query) { q.Projects = []string{"web"} }, "plain,slow page"},
		{"two projects", func(q *Query) { q.Projects = []string{"web", "lys"}; q.Limit = 1 }, "plain"},
		{"service", func(q *Query) { q.Services = []string{"bot"} }, "lookup 100%_done,Twitch: connection refused"},
		{"container", func(q *Query) { q.Containers = []string{"web"} }, "plain,slow page"},
		{"version", func(q *Query) { q.Version = "v1" }, "lookup 100%_done,Twitch: connection refused"},
		{"level", func(q *Query) { q.MinLevel = 30 }, "slow page,Twitch: connection refused"},
		{"stream", func(q *Query) { q.Stream = 1 }, "plain"},
		{"text any case", func(q *Query) { q.Text = "CONNECTION" }, "Twitch: connection refused"},
		{"percent is literal", func(q *Query) { q.Text = "100%" }, "lookup 100%_done"},
		{"underscore is literal", func(q *Query) { q.Text = "t_w" }, ""},
		{"wildcard is literal", func(q *Query) { q.Text = "%" }, "lookup 100%_done"},
		{"quote is safe", func(q *Query) { q.Text = "'; DROP TABLE blackbox.records; --" }, ""},
		{"text in attrs", func(q *Query) { q.Text = "abc" }, "plain"},
		{"field", func(q *Query) { q.Fields = []Field{{"request_id", "abc"}} }, "plain"},
		{"number field", func(q *Query) { q.Fields = []Field{{"count", "5"}} }, "plain"},
		{"field mismatch", func(q *Query) { q.Fields = []Field{{"request_id", "x"}} }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := wide()
			tt.edit(&q)
			res, err := s.Search(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			if got := messages(res.Rows); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}

	q := wide()
	q.MinLevel = 30
	res, err := s.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if res.UnknownLeftOut != 1 {
		t.Errorf("unknown left out = %d, want 1", res.UnknownLeftOut)
	}
	if r := res.Rows[0]; r.Project != "web" || r.Service != "site" || r.Container != "web" || r.Version != "v2" || r.Image != "img" {
		t.Errorf("row = %+v", r)
	}
}

func TestPagesStayStableWhileLinesArrive(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tr := add(t, s, "lys", "lys", "lys", "v1")
	write(t, s, tr, day, 20, "a", "b", "c", "d", "e")

	q := wide()
	q.Limit = 2
	first, err := s.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if messages(first.Rows) != "e,d" || !first.More {
		t.Fatalf("first page = %q, more %v", messages(first.Rows), first.More)
	}

	write(t, s, tr, day.Add(time.Minute), 20, "newer")
	last := first.Rows[len(first.Rows)-1]
	q.After = &Cursor{Time: last.Time, ID: last.ID}
	second, err := s.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if messages(second.Rows) != "c,b" || !second.More {
		t.Fatalf("second page = %q", messages(second.Rows))
	}
	last = second.Rows[len(second.Rows)-1]
	q.After = &Cursor{Time: last.Time, ID: last.ID}
	third, err := s.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if messages(third.Rows) != "a" || third.More {
		t.Fatalf("third page = %q, more %v", messages(third.Rows), third.More)
	}
}

func TestRecordAndContext(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	lys := add(t, s, "lys", "lys", "lys", "v1")
	web := add(t, s, "web", "web", "web", "v1")
	write(t, s, lys, day, 20, "a", "b", "c", "d", "e")
	write(t, s, web, day, 20, "other")

	q := wide()
	q.Text = "c"
	res, err := s.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	id := res.Rows[0].ID

	row, err := s.Record(ctx, id)
	if err != nil || row.Message != "c" {
		t.Fatalf("record = %+v, %v", row, err)
	}
	around, err := s.Context(ctx, id, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := messages(around); got != "b,c,d" {
		t.Errorf("context = %q", got)
	}
	around, err = s.Context(ctx, id, 20, 20)
	if err != nil {
		t.Fatal(err)
	}
	if got := messages(around); got != "a,b,c,d,e" {
		t.Errorf("context = %q: lines of another container mustn't appear", got)
	}
	if _, err := s.Record(ctx, 0); err != ErrNoRecord {
		t.Errorf("err = %v", err)
	}
	if _, err := s.Context(ctx, 0, 1, 1); err != ErrNoRecord {
		t.Errorf("err = %v", err)
	}
}

func TestSources(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	a := add(t, s, "lys", "lys", "bot", "v1")
	add(t, s, "new", "lys", "bot", "v2")
	add(t, s, "ext", "", "ext", "")
	write(t, s, a, day, 20, "a", "b")
	if err := s.Report(ctx, CollectorStatus{HeartbeatAt: day, Version: "v"}, []StreamReport{{ID: a.ID, State: "open"}}, nil); err != nil {
		t.Fatal(err)
	}

	list, err := s.Sources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("sources = %+v", list)
	}
	if list[0].Service != "ext" || len(list[0].Versions) != 0 || list[0].Newest != nil {
		t.Errorf("first = %+v", list[0])
	}
	bot := list[1]
	if bot.Lines != 2 || fmt.Sprint(bot.Versions) != "[v1 v2]" || bot.OpenStreams != 1 || bot.Newest == nil {
		t.Errorf("bot = %+v", bot)
	}
}

func TestDamageMovesTheCheckpointAndNotesTheGap(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tr := add(t, s, "lys", "lys", "lys", "v1")
	now := day.Add(time.Hour)
	err := s.SetDamaged(ctx, tr.ID, Gap{Kind: GapLogDamaged, ContainerID: &tr.ID, From: day, To: now, Detail: "stuck", NotedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := s.Tracked(ctx)
	if !list[0].Damaged || list[0].Checkpoint != now.UnixNano() {
		t.Errorf("tracked = %+v", list[0])
	}
	gaps, err := s.Gaps(ctx, now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 1 || gaps[0].Kind != GapLogDamaged || gaps[0].Container != "lys" {
		t.Errorf("gaps = %+v", gaps)
	}
}

func TestStatus(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	r := reader(t)
	st, err := r.ReadStatus(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if st.Reported {
		t.Error("nothing has reported yet")
	}

	tr := add(t, s, "lys", "lys", "lys", "v1")
	b := Batch{Records: []Record{{ContainerID: tr.ID, Time: day, Message: "x", Redacted: true}}}
	if err := s.Save(ctx, b, day); err != nil {
		t.Fatal(err)
	}
	if err := s.AddGap(ctx, Gap{Kind: GapCollectorDown, From: day, To: day, Detail: "x", NotedAt: day}); err != nil {
		t.Fatal(err)
	}
	report := CollectorStatus{HeartbeatAt: day, Version: "v1", StreamsOpen: 1, StreamsRefused: 2, LagSeconds: 1.5, LastError: "oops"}
	if err := s.Report(ctx, report, []StreamReport{{ID: tr.ID, State: "failed", LastError: "proxy: x"}}, []int32{tr.ID}); err != nil {
		t.Fatal(err)
	}
	if at, ok, err := s.LastHeartbeat(ctx); err != nil || !ok || !at.Equal(day) {
		t.Errorf("heartbeat = %v, %v, %v", at, ok, err)
	}
	st, err = r.ReadStatus(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Reported || st.Version != "v1" || st.StreamsOpen != 1 || st.StreamsRefused != 2 || st.LastError != "oops" {
		t.Errorf("status = %+v", st)
	}
	if st.Redacted24h != 1 || st.Gaps24h != 1 || st.Oldest == nil {
		t.Errorf("figures = %+v", st)
	}
	if len(st.Problems) != 1 || st.Problems[0].Container != "lys" || st.Problems[0].State != "failed" {
		t.Errorf("problems = %+v", st.Problems)
	}
}

func TestRetention(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.Report(ctx, CollectorStatus{HeartbeatAt: day, Version: "v"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	now := day.Add(31 * 24 * time.Hour)
	gone := add(t, s, "old", "old", "old", "v1")
	kept := add(t, s, "lys", "lys", "lys", "v1")
	write(t, s, gone, day, 20, "too old")
	write(t, s, kept, now.Add(-time.Hour), 20, "recent")
	if err := s.SetRemoved(ctx, gone.ID, day); err != nil {
		t.Fatal(err)
	}
	if err := s.AddGap(ctx, Gap{Kind: GapCollectorDown, From: day, To: day, Detail: "x", NotedAt: day}); err != nil {
		t.Fatal(err)
	}

	deleted, stored, err := s.Retain(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 || stored == 0 {
		t.Errorf("deleted %d, stored %d", deleted, stored)
	}
	q := Query{From: day.Add(-time.Hour), To: now, Limit: 100}
	res, _ := s.Search(ctx, q)
	if messages(res.Rows) != "recent" {
		t.Errorf("rows = %q", messages(res.Rows))
	}
	var n int
	s.pool.QueryRow(ctx, "SELECT count(*) FROM blackbox.containers").Scan(&n)
	if n != 1 {
		t.Errorf("containers = %d: the removed one should be gone", n)
	}
	s.pool.QueryRow(ctx, "SELECT count(*) FROM blackbox.gaps").Scan(&n)
	if n != 0 {
		t.Errorf("gaps = %d", n)
	}
}

func TestDeleteOldestGoesInOrder(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tr := add(t, s, "lys", "lys", "lys", "v1")
	write(t, s, tr, day, 20, "a", "b", "c", "d")

	before, err := s.storedBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.deleteOldest(ctx, before/2); err != nil {
		t.Fatal(err)
	}
	res, _ := s.Search(ctx, Query{From: day.Add(-time.Hour), To: day.Add(time.Hour), Limit: 100, Oldest: true})
	if got := messages(res.Rows); got != "c,d" {
		t.Errorf("left = %q", got)
	}
}

func TestReaderCannotWrite(t *testing.T) {
	open(t)
	r := reader(t)
	ctx := context.Background()
	if _, err := r.pool.Exec(ctx, "DELETE FROM blackbox.records"); err == nil {
		t.Error("the reader must not be able to delete")
	}
	if _, err := r.pool.Exec(ctx, "UPDATE blackbox.containers SET name = 'x'"); err == nil {
		t.Error("the reader must not be able to update")
	}
}

func TestAttrsComeBackAsJSON(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tr := add(t, s, "lys", "lys", "lys", "v1")
	b := Batch{Records: []Record{{ContainerID: tr.ID, Time: day, Message: "x", Attrs: []byte(`{"a":"b"}`)}}}
	if err := s.Save(ctx, b, day); err != nil {
		t.Fatal(err)
	}
	res, err := s.Search(ctx, wide())
	if err != nil {
		t.Fatal(err)
	}
	var attrs map[string]string
	if err := json.Unmarshal(res.Rows[0].Attrs, &attrs); err != nil || attrs["a"] != "b" {
		t.Errorf("attrs = %s, %v", res.Rows[0].Attrs, err)
	}
}

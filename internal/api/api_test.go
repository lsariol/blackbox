package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/blackbox/internal/lines"
	"github.com/lsariol/blackbox/internal/store"
)

const token = "0123456789abcdef0123456789abcdef"

var now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// fakeReader answers with fixed data and remembers the last search.
type fakeReader struct {
	result store.Result
	row    store.Row
	rows   []store.Row
	status store.Status
	err    error
	slow   bool
	last   store.Query
}

func (f *fakeReader) wait(ctx context.Context) error {
	if f.slow {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.err
}

func (f *fakeReader) Search(ctx context.Context, q store.Query) (store.Result, error) {
	f.last = q
	return f.result, f.wait(ctx)
}
func (f *fakeReader) Record(ctx context.Context, id int64) (store.Row, error) { return f.row, f.err }
func (f *fakeReader) Context(ctx context.Context, id int64, b, a int) ([]store.Row, error) {
	return f.rows, f.err
}
func (f *fakeReader) Sources(ctx context.Context) ([]store.Source, error) {
	return []store.Source{{Project: "lys", Service: "bot", Versions: []string{"v1"}, Lines: 3, OpenStreams: 1}, {Service: "ext"}}, f.err
}
func (f *fakeReader) Gaps(ctx context.Context, from, to time.Time) ([]store.GapRow, error) {
	return []store.GapRow{{Kind: store.GapLogDamaged, Container: "web", From: now, To: now, Detail: "x", NotedAt: now}}, f.err
}
func (f *fakeReader) ReadStatus(ctx context.Context, at time.Time) (store.Status, error) {
	return f.status, f.err
}
func (f *fakeReader) Migrations(ctx context.Context) ([]store.Migration, error) {
	return []store.Migration{{Version: 1, Name: "00001_initial", AppliedAt: &now}}, f.err
}
func (f *fakeReader) CheckSchemaVersion(ctx context.Context) error { return nil }
func (f *fakeReader) Ping(ctx context.Context) error               { return f.err }

func newServer(t *testing.T, db Reader) *Server {
	t.Helper()
	s, err := New(Options{
		Version: "v0.0.0",
		Getenv:  func(key string) string { return map[string]string{"BLACKBOX_CLI_TOKEN": token}[key] },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if db != nil {
		s.db, s.dbErr = db, nil
	}
	return s
}

func call(t *testing.T, s *Server, path, tok string) (int, string, http.Header) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), rec.Header()
}

func decode[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	return v
}

func TestRefusesToStartWithoutAToken(t *testing.T) {
	for _, value := range []string{"", "short"} {
		_, err := New(Options{Getenv: func(string) string { return value }})
		if err == nil || !strings.Contains(err.Error(), "BLACKBOX_CLI_TOKEN") {
			t.Errorf("token %q: err = %v", value, err)
		}
	}
}

func TestAuth(t *testing.T) {
	s := newServer(t, &fakeReader{})
	for _, path := range []string{"/v1/logs", "/v1/logs/1", "/v1/logs/1/context", "/v1/sources", "/v1/gaps", "/v1/status", "/v1/migrations"} {
		if code, _, _ := call(t, s, path, ""); code != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d", path, code)
		}
		if code, _, h := call(t, s, path, "wrong"); code != http.StatusUnauthorized || h.Get("WWW-Authenticate") == "" {
			t.Errorf("%s with a wrong token: %d", path, code)
		}
	}
}

func TestConsumerWithoutTheScopeIsRefused(t *testing.T) {
	c := consumer{Name: "other"}
	if c.may(logsRead) {
		t.Error("a consumer without the scope may not read")
	}
}

func TestHealthzNeedsNoTokenAndSaysLittle(t *testing.T) {
	code, body, _ := call(t, newServer(t, &fakeReader{}), "/healthz", "")
	if code != http.StatusOK || !strings.Contains(body, `"ok"`) {
		t.Errorf("%d: %s", code, body)
	}
	code, body, _ = call(t, newServer(t, nil), "/healthz", "")
	if code != http.StatusServiceUnavailable || strings.Contains(body, "connected") {
		t.Errorf("%d: %s", code, body)
	}
}

func TestSearchAnswer(t *testing.T) {
	db := &fakeReader{result: store.Result{
		Rows: []store.Row{
			{ID: 18234, Time: now, Project: "lys", Service: "bot", Container: "lys", Image: "lys:1", Version: "v0.3.0", Stream: lines.Stderr,
				Level: lines.LevelError, Message: "Twitch: refused", Attrs: []byte(`{"operation":"chat.send"}`)},
			{ID: 7, Time: now, Service: "ext", Message: "plain"},
		},
		More: true, UnknownLeftOut: 12,
	}}
	code, body, h := call(t, newServer(t, db), "/v1/logs?project=lys&level=warn", token)
	if code != http.StatusOK {
		t.Fatalf("%d: %s", code, body)
	}
	if h.Get("Cache-Control") != "no-store" {
		t.Error("answers mustn't be cached")
	}
	answer := decode[LogsAnswer](t, body)
	if len(answer.Records) != 2 || answer.UnknownLeftOut != 12 || answer.NextCursor == nil {
		t.Fatalf("answer = %+v", answer)
	}
	r := answer.Records[0]
	if r.ID != 18234 || *r.Project != "lys" || *r.Version != "v0.3.0" || r.Stream != "stderr" || r.Level != "error" || string(r.Attrs) != `{"operation":"chat.send"}` {
		t.Errorf("record = %+v", r)
	}
	if plain := answer.Records[1]; plain.Project != nil || plain.Version != nil || plain.Level != "unknown" {
		t.Errorf("empty values must be null: %+v", plain)
	}
	if !strings.Contains(body, `"attrs":null`) {
		t.Errorf("missing attrs must be null: %s", body)
	}

	cursor, err := decodeCursor(*answer.NextCursor)
	if err != nil || cursor.ID != 7 || !cursor.Time.Equal(now) {
		t.Errorf("cursor = %+v, %v", cursor, err)
	}
}

func TestParseQuery(t *testing.T) {
	q, reason := parseQuery(url.Values{}, now)
	if reason != "" || !q.To.Equal(now) || !q.From.Equal(now.Add(-time.Hour)) || q.Limit != defaultLimit || q.MinLevel != 0 {
		t.Errorf("defaults = %+v, %q", q, reason)
	}

	cursor := encodeCursor(store.Cursor{Time: now, ID: 9})
	v := url.Values{
		"from": {"2026-11-01T00:00:00Z"}, "to": {"2026-11-02T00:00:00Z"}, "project": {"a", "b"}, "service": {"s"}, "container": {"c"},
		"version": {"v1"}, "level": {"warn"}, "stream": {"stderr"}, "q": {"text"}, "field": {"a=b=c", "d="},
		"order": {"oldest"}, "limit": {"5"}, "cursor": {cursor},
	}
	q, reason = parseQuery(v, now)
	if reason != "" {
		t.Fatal(reason)
	}
	if len(q.Projects) != 2 || q.Services[0] != "s" || q.Containers[0] != "c" || q.Version != "v1" || q.MinLevel != lines.LevelWarn ||
		q.Stream != lines.Stderr || q.Text != "text" || !q.Oldest || q.Limit != 5 || q.After == nil || q.After.ID != 9 {
		t.Errorf("query = %+v", q)
	}
	if q.Fields[0] != (store.Field{Key: "a", Value: "b=c"}) || q.Fields[1] != (store.Field{Key: "d", Value: ""}) {
		t.Errorf("fields = %+v", q.Fields)
	}

	bad := []string{
		"from=yesterday", "to=soon", "from=2026-11-03T00:00:00Z&to=2026-11-02T00:00:00Z", "level=unknown", "level=loud", "stream=both",
		"field=nokey", "field==x", "order=random", "limit=0", "limit=1001", "limit=x", "cursor=!!!", "cursor=abcd", "nonsense=1",
		"q=" + strings.Repeat("x", maxText+1), "field=a=1&field=a=2&field=a=3&field=a=4&field=a=5&field=a=6",
	}
	for _, query := range bad {
		values, err := url.ParseQuery(query)
		if err != nil {
			t.Fatal(err)
		}
		if _, reason := parseQuery(values, now); reason == "" {
			t.Errorf("%q should be refused", query)
		}
	}
}

func TestBadSearchIs400(t *testing.T) {
	code, body, _ := call(t, newServer(t, &fakeReader{}), "/v1/logs?level=loud", token)
	if code != http.StatusBadRequest || !strings.Contains(body, "level must be") {
		t.Errorf("%d: %s", code, body)
	}
}

func TestRecordAndContext(t *testing.T) {
	db := &fakeReader{row: store.Row{ID: 5, Time: now, Service: "s", Message: "m"}, rows: []store.Row{{ID: 4}, {ID: 5}}}
	s := newServer(t, db)
	code, body, _ := call(t, s, "/v1/logs/5", token)
	if code != http.StatusOK || decode[Record](t, body).Message != "m" {
		t.Errorf("%d: %s", code, body)
	}
	code, body, _ = call(t, s, "/v1/logs/5/context?before=1&after=0", token)
	answer := decode[ContextAnswer](t, body)
	if code != http.StatusOK || answer.Target != 5 || len(answer.Records) != 2 {
		t.Errorf("%d: %s", code, body)
	}
	for _, path := range []string{"/v1/logs/x", "/v1/logs/0", "/v1/logs/-1", "/v1/logs/x/context", "/v1/logs/1/context?before=201", "/v1/logs/1/context?after=x"} {
		if code, _, _ := call(t, s, path, token); code != http.StatusBadRequest {
			t.Errorf("%s: %d", path, code)
		}
	}
	db.err = store.ErrNoRecord
	if code, _, _ := call(t, s, "/v1/logs/5", token); code != http.StatusNotFound {
		t.Errorf("a missing record: %d", code)
	}
}

func TestOtherEndpoints(t *testing.T) {
	s := newServer(t, &fakeReader{})
	_, body, _ := call(t, s, "/v1/sources", token)
	src := decode[SourcesAnswer](t, body).Sources
	if len(src) != 2 || *src[0].Project != "lys" || src[1].Project != nil || src[1].Versions == nil {
		t.Errorf("sources = %s", body)
	}
	_, body, _ = call(t, s, "/v1/gaps", token)
	if g := decode[GapsAnswer](t, body).Gaps; len(g) != 1 || *g[0].Container != "web" {
		t.Errorf("gaps = %s", body)
	}
	if code, _, _ := call(t, s, "/v1/gaps?from=x", token); code != http.StatusBadRequest {
		t.Errorf("bad from: %d", code)
	}
	_, body, _ = call(t, s, "/v1/migrations", token)
	if m := decode[MigrationsAnswer](t, body).Migrations; len(m) != 1 || m[0].Name != "00001_initial" {
		t.Errorf("migrations = %s", body)
	}
	if code, _, _ := call(t, s, "/nope", token); code != http.StatusNotFound {
		t.Errorf("unknown path: %d", code)
	}
}

func TestStatusAttention(t *testing.T) {
	healthy := store.Status{Reported: true, HeartbeatAt: now.Add(-5 * time.Second), Version: "v1", StreamsOpen: 12, StoredBytes: 1 << 20}
	tests := []struct {
		name string
		edit func(*store.Status)
		want string
	}{
		{"healthy", func(s *store.Status) {}, ""},
		{"gaps alone are no alarm", func(s *store.Status) { s.Gaps24h = 3 }, ""},
		{"never reported", func(s *store.Status) { s.Reported = false }, "hasn't reported"},
		{"stale", func(s *store.Status) { s.HeartbeatAt = now.Add(-2 * time.Minute) }, "2m0s ago"},
		{"error", func(s *store.Status) { s.LastError = "database: down" }, "database: down"},
		{"refused", func(s *store.Status) { s.StreamsRefused = 2 }, "refused"},
		{"lag", func(s *store.Status) { s.LagSeconds = 120 }, "120 s behind"},
		{"storage", func(s *store.Status) { s.StoredBytes = store.MaxBytes * 9 / 10 }, "storage is at 8"},
		{"damaged", func(s *store.Status) { s.Problems = []store.StreamProblem{{Container: "web", Damaged: true}} }, `"web" has a damaged log`},
		{"failed stream", func(s *store.Status) {
			s.Problems = []store.StreamProblem{{Container: "web", State: "failed", LastError: "proxy: x"}}
		}, "proxy: x"},
		{"refused stream", func(s *store.Status) { s.Problems = []store.StreamProblem{{Container: "web", State: "refused"}} }, "stream refused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := healthy
			tt.edit(&st)
			answer := buildStatus(st, now)
			got := strings.Join(answer.Attention, "; ")
			if tt.want == "" && got != "" || tt.want != "" && !strings.Contains(got, tt.want) {
				t.Errorf("attention = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDatabaseProblems(t *testing.T) {
	code, body, _ := call(t, newServer(t, nil), "/v1/logs", token)
	if code != http.StatusServiceUnavailable {
		t.Errorf("%d: %s", code, body)
	}
	code, body, _ = call(t, newServer(t, &fakeReader{err: errors.New("database: secret detail")}), "/v1/logs", token)
	if code != http.StatusServiceUnavailable || strings.Contains(body, "secret detail") {
		t.Errorf("%d: %s", code, body)
	}
}

func TestClientAgainstTheServer(t *testing.T) {
	db := &fakeReader{
		result: store.Result{Rows: []store.Row{{ID: 3, Time: now, Service: "s", Message: "hello"}}},
		row:    store.Row{ID: 3, Message: "one"},
		rows:   []store.Row{{ID: 3}},
		status: store.Status{Reported: true, HeartbeatAt: now, Version: "v1"},
	}
	srv := httptest.NewServer(newServer(t, db).Handler())
	defer srv.Close()
	ctx := context.Background()

	c := NewClient(srv.URL, token)
	logs, err := c.Logs(ctx, url.Values{"q": {"a b&c"}})
	if err != nil || len(logs.Records) != 1 || db.last.Text != "a b&c" {
		t.Errorf("logs = %+v, %v, %q", logs, err, db.last.Text)
	}
	if r, err := c.Record(ctx, 3); err != nil || r.Message != "one" {
		t.Errorf("record = %+v, %v", r, err)
	}
	if a, err := c.Context(ctx, 3, 1, 1); err != nil || a.Target != 3 {
		t.Errorf("context = %+v, %v", a, err)
	}
	if s, err := c.Sources(ctx); err != nil || len(s.Sources) != 2 {
		t.Errorf("sources = %+v, %v", s, err)
	}
	if g, err := c.Gaps(ctx, now); err != nil || len(g.Gaps) != 1 {
		t.Errorf("gaps = %+v, %v", g, err)
	}
	if st, err := c.Status(ctx); err != nil || len(st.Attention) != 0 {
		t.Errorf("status = %+v, %v", st, err)
	}
	if m, err := c.Migrations(ctx); err != nil || len(m.Migrations) != 1 {
		t.Errorf("migrations = %+v, %v", m, err)
	}

	_, err = NewClient(srv.URL, "wrong").Status(ctx)
	if err == nil || !strings.Contains(err.Error(), "unknown token") {
		t.Errorf("err = %v", err)
	}
	srv.Close()
	_, err = c.Status(ctx)
	if err == nil || !strings.Contains(err.Error(), "docker exec") {
		t.Errorf("err = %v", err)
	}
}

func TestSlowQueryAsksForANarrowerSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the query timeout")
	}
	code, body, _ := call(t, newServer(t, &fakeReader{slow: true}), "/v1/logs", token)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "shorter time range") {
		t.Errorf("%d: %s", code, body)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 2, 1, 2, 3, 456789000, time.UTC)
	for _, id := range []int64{1, 1 << 40} {
		got, err := decodeCursor(encodeCursor(store.Cursor{Time: at, ID: id}))
		if err != nil || got.ID != id || !got.Time.Equal(at) {
			t.Errorf("%d: %+v, %v", id, got, err)
		}
	}
}

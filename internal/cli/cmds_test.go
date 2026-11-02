package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/blackbox/internal/api"
)

var now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func ptr(s string) *string { return &s }

// fakeAPI answers with fixed data and remembers the last search.
type fakeAPI struct {
	logs     api.LogsAnswer
	context  api.ContextAnswer
	record   api.Record
	sources  api.SourcesAnswer
	gaps     api.GapsAnswer
	status   api.StatusAnswer
	migrated api.MigrationsAnswer
	err      error
	query    url.Values
}

func (f *fakeAPI) Logs(ctx context.Context, q url.Values) (api.LogsAnswer, error) {
	f.query = q
	return f.logs, f.err
}
func (f *fakeAPI) Record(ctx context.Context, id int64) (api.Record, error) { return f.record, f.err }
func (f *fakeAPI) Context(ctx context.Context, id int64, b, a int) (api.ContextAnswer, error) {
	return f.context, f.err
}
func (f *fakeAPI) Sources(ctx context.Context) (api.SourcesAnswer, error) { return f.sources, f.err }
func (f *fakeAPI) Gaps(ctx context.Context, from time.Time) (api.GapsAnswer, error) {
	return f.gaps, f.err
}
func (f *fakeAPI) Status(ctx context.Context) (api.StatusAnswer, error) { return f.status, f.err }
func (f *fakeAPI) Migrations(ctx context.Context) (api.MigrationsAnswer, error) {
	return f.migrated, f.err
}

func newCLI(f *fakeAPI) *CLI {
	return New(Options{
		Env: "dev", Version: "v0.0.0",
		API: func() (API, error) { return f, nil },
		Now: func() time.Time { return now },
	})
}

func exec(t *testing.T, f *fakeAPI, line string) (string, string, error) {
	t.Helper()
	o, e := capture(t)
	args, err := splitArgs(line)
	if err != nil {
		t.Fatal(err)
	}
	err = newCLI(f).Exec(context.Background(), args)
	return o.String(), e.String(), err
}

func record() api.Record {
	return api.Record{ID: 18234, Time: now, Project: ptr("lys"), Service: "bot", Container: "lys", Image: "lys:1", Version: ptr("v0.3.0"),
		Stream: "stderr", Level: "error", Message: "Twitch: refused", Attrs: json.RawMessage(`{"operation":"chat.send","count":5}`)}
}

func TestParseSearch(t *testing.T) {
	p, err := parseSearch([]string{"connection", "refused", "--project", "lys", "--project=web", "--level", "error", "--since", "6h",
		"--service", "bot", "--container", "c", "--version", "v1", "--stream", "stderr", "--field", "a=b", "--oldest", "--limit", "5", "--cursor", "X", "--json"}, now)
	if err != nil {
		t.Fatal(err)
	}
	q := p.query
	if q.Get("q") != "connection refused" || len(q["project"]) != 2 || q.Get("level") != "error" || q.Get("service") != "bot" || q.Get("container") != "c" ||
		q.Get("version") != "v1" || q.Get("stream") != "stderr" || q.Get("field") != "a=b" || q.Get("order") != "oldest" || q.Get("limit") != "5" || q.Get("cursor") != "X" || !p.asJSON {
		t.Errorf("query = %v", q)
	}
	if q.Get("from") != now.Add(-6*time.Hour).Format(time.RFC3339Nano) || q.Get("to") != now.Format(time.RFC3339Nano) {
		t.Errorf("range = %s to %s", q.Get("from"), q.Get("to"))
	}

	p, err = parseSearch(nil, now)
	if err != nil || p.query.Get("from") != now.Add(-time.Hour).Format(time.RFC3339Nano) {
		t.Errorf("the default range is an hour: %v, %v", p.query, err)
	}

	p, err = parseSearch([]string{"--from", "2026-11-02T10:00:00Z"}, now)
	if err != nil || p.query.Has("to") {
		t.Errorf("from alone: %v, %v", p.query, err)
	}
	p, err = parseSearch([]string{"--from", "2026-11-02 10:00", "--to", "2026-11-02 11:00:30"}, now)
	if err != nil || !p.query.Has("from") || !p.query.Has("to") {
		t.Errorf("server times: %v, %v", p.query, err)
	}

	for _, args := range [][]string{
		{"--nope"}, {"--level"}, {"--since", "soon"}, {"--since", "5"}, {"--since", "0h"}, {"--from", "yesterday"},
		{"--since", "1h", "--from", "2026-11-02"},
	} {
		_, err := parseSearch(args, now)
		var usage usageError
		if !errors.As(err, &usage) {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}

func TestSearchPrintsLines(t *testing.T) {
	f := &fakeAPI{logs: api.LogsAnswer{Records: []api.Record{record()}, NextCursor: ptr("MTgy"), UnknownLeftOut: 12}}
	o, e, err := exec(t, f, `search --project lys --level error`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"18234", now.Local().Format(lineTime), "error", "lys/bot", "v0.3.0", "Twitch: refused", "count=5", "operation=chat.send"} {
		if !strings.Contains(o, want) {
			t.Errorf("stdout is missing %q: %s", want, o)
		}
	}
	for _, want := range []string{"1 line", "12 lines in this range have no level and were left out", "--cursor MTgy"} {
		if !strings.Contains(e, want) {
			t.Errorf("stderr is missing %q: %s", want, e)
		}
	}
}

func TestSearchJSONIsDataOnStdout(t *testing.T) {
	f := &fakeAPI{logs: api.LogsAnswer{Records: []api.Record{record(), record()}}}
	o, _, err := exec(t, f, "search --json")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(o), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout = %s", o)
	}
	var r api.Record
	if err := json.Unmarshal([]byte(lines[0]), &r); err != nil || r.ID != 18234 {
		t.Errorf("line = %s: %v", lines[0], err)
	}
}

func TestLogTextCannotReachTheTerminal(t *testing.T) {
	r := record()
	r.Message = "bad \x1b[31m text\x07"
	r.Attrs = json.RawMessage(`{"a":"\u001b]0;title\u0007"}`)
	f := &fakeAPI{logs: api.LogsAnswer{Records: []api.Record{r}}, record: r,
		context: api.ContextAnswer{Target: r.ID, Records: []api.Record{r}}}
	for _, line := range []string{"search", "show 1", "context 1", "search --json"} {
		o, _, err := exec(t, f, line)
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(o, "\x1b\x07") {
			t.Errorf("%s: a control character got through: %q", line, o)
		}
	}
}

func TestSingleLevelWording(t *testing.T) {
	f := &fakeAPI{logs: api.LogsAnswer{UnknownLeftOut: 1}}
	_, e, err := exec(t, f, "search --level warn")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e, "1 line in this range has no level and was left out") || !strings.Contains(e, "0 lines") {
		t.Errorf("stderr = %s", e)
	}
}

func TestShow(t *testing.T) {
	r := record()
	r.Truncated, r.Redacted = true, true
	o, _, err := exec(t, &fakeAPI{record: r}, "show 18234")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ID:", "18234", "Project:", "lys", "Image:", "lys:1", "operation:", "chat.send", "Cut short:", "Redacted:"} {
		if !strings.Contains(o, want) {
			t.Errorf("missing %q: %s", want, o)
		}
	}
	for _, line := range []string{"show", "show x", "show 1 2"} {
		_, _, err := exec(t, &fakeAPI{}, line)
		var usage usageError
		if !errors.As(err, &usage) {
			t.Errorf("%s: err = %v", line, err)
		}
	}
}

func TestContextMarksTheRecord(t *testing.T) {
	a, b := record(), record()
	b.ID = 18235
	f := &fakeAPI{context: api.ContextAnswer{Target: 18235, Records: []api.Record{a, b}}}
	o, e, err := exec(t, f, "context 18235 --before 1 --after 1")
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(o), "\n")
	if len(rows) != 2 || strings.Contains(rows[0], ">") || !strings.HasPrefix(rows[1], ">") {
		t.Errorf("rows = %q", rows)
	}
	if !strings.Contains(e, "2 lines") {
		t.Errorf("stderr = %s", e)
	}
	for _, line := range []string{"context", "context x", "context 1 --before", "context 1 --nope 2", "context 1 --before x"} {
		_, _, err := exec(t, &fakeAPI{}, line)
		var usage usageError
		if !errors.As(err, &usage) {
			t.Errorf("%s: err = %v", line, err)
		}
	}
}

func TestStatusHealthy(t *testing.T) {
	st := api.StatusAnswer{Version: ptr("v0.0.0"), HeartbeatAt: &now, LastSavedAt: &now, StreamsOpen: 12, StreamsRead: 1,
		StoredBytes: 91 << 20, MaxBytes: 1 << 30, Oldest: &now, Redacted24h: 14}
	o, e, err := exec(t, &fakeAPI{status: st}, "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Collector:", "version v0.0.0", "12 open, 1 stopped container read", "under 2 s", "91.0 MiB of 1.0 GiB", "14 lines", "none in the last 24 h"} {
		if !strings.Contains(o, want) {
			t.Errorf("missing %q: %s", want, o)
		}
	}
	if e != "" {
		t.Errorf("healthy status should print nothing extra: %s", e)
	}
}

func TestStatusNeedsAttention(t *testing.T) {
	st := api.StatusAnswer{Attention: []string{"one", "two"}, Gaps24h: 2, LagSeconds: 90}
	o, e, err := exec(t, &fakeAPI{status: st}, "status")
	if err == nil {
		t.Fatal("status should fail when something needs attention")
	}
	if !strings.Contains(e, "Needs attention: one; two.") {
		t.Errorf("stderr = %s", e)
	}
	if !strings.Contains(o, "hasn't reported yet") || !strings.Contains(o, "90 s") || !strings.Contains(o, "2 in the last 24 h") {
		t.Errorf("stdout = %s", o)
	}
	var printed strings.Builder
	report(err)
	if printed.Len() != 0 {
		t.Error("the error mustn't be printed twice")
	}
}

func TestSourcesAndGaps(t *testing.T) {
	f := &fakeAPI{
		sources: api.SourcesAnswer{Sources: []api.Source{{Project: ptr("lys"), Service: "bot", Versions: []string{"v1", "v2"}, Newest: &now, Lines: 3, OpenStreams: 1}, {Service: "ext"}}},
		gaps:    api.GapsAnswer{Gaps: []api.Gap{{Kind: "log_damaged", Container: ptr("web"), From: now, To: now, Detail: "stuck"}}},
	}
	o, e, err := exec(t, f, "sources")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(o, "v1, v2") || !strings.Contains(o, "open") || !strings.Contains(o, "closed") || !strings.Contains(e, "2 sources") {
		t.Errorf("%s / %s", o, e)
	}
	o, e, err = exec(t, f, "gaps --since 30d")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(o, "log_damaged") || !strings.Contains(o, "web") || !strings.Contains(e, "1 gap") {
		t.Errorf("%s / %s", o, e)
	}
	_, _, err = exec(t, f, "gaps --since soon")
	var usage usageError
	if !errors.As(err, &usage) {
		t.Errorf("err = %v", err)
	}
}

func TestSetupCommands(t *testing.T) {
	f := &fakeAPI{migrated: api.MigrationsAnswer{Migrations: []api.Migration{{Version: 1, Name: "00001_initial", AppliedAt: &now}, {Version: 2, Name: "00002_role_grants"}}}}
	o, e, err := exec(t, f, "migrate status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(o, "pending") || !strings.Contains(e, "1 migration is pending") {
		t.Errorf("%s / %s", o, e)
	}

	o2, e2 := capture(t)
	c := New(Options{Consumers: func() []api.ConsumerInfo {
		return []api.ConsumerInfo{{Name: "cli", Key: "BLACKBOX_CLI_TOKEN", Scopes: []string{"logs:read"}, Token: "missing"}}
	}})
	if err := c.Exec(context.Background(), []string{"consumers"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(o2.String(), "BLACKBOX_CLI_TOKEN") || !strings.Contains(e2.String(), "generate BLACKBOX_CLI_TOKEN 64") {
		t.Errorf("%s / %s", o2, e2)
	}
}

func TestAPIErrorsArePassedOn(t *testing.T) {
	f := &fakeAPI{err: errors.New("API: the database isn't ready yet")}
	for _, line := range []string{"search", "show 1", "context 1", "status", "sources", "gaps", "migrate status"} {
		_, _, err := exec(t, f, line)
		if err == nil || !strings.Contains(err.Error(), "database") {
			t.Errorf("%s: err = %v", line, err)
		}
	}
}

func TestWithoutTheAPISaysWhereToRun(t *testing.T) {
	capture(t)
	c := New(Options{})
	err := c.Exec(context.Background(), []string{"status"})
	if err == nil || !strings.Contains(err.Error(), "docker exec") {
		t.Errorf("err = %v", err)
	}
}

func TestTabCompletion(t *testing.T) {
	f := &fakeAPI{sources: api.SourcesAnswer{Sources: []api.Source{
		{Project: ptr("lys"), Service: "bot", Versions: []string{"v1"}},
		{Project: ptr("lighthouse"), Service: "lighthouse"},
	}}}
	c := newCLI(f)
	tests := []struct {
		line string
		want string
	}{
		{"sea", "search "},
		{"search --pro", "search --project "},
		{"search --project ly", "search --project lys "},
		{"search --service b", "search --service bot "},
		{"search --version v", "search --version v1 "},
		{"help sta", "help status "},
	}
	for _, tt := range tests {
		got, _, ok := c.complete(tt.line, len(tt.line), '\t')
		if !ok || got != tt.want {
			t.Errorf("%q completed to %q, %v", tt.line, got, ok)
		}
	}
	if _, _, ok := c.complete("search text", 11, '\t'); ok {
		t.Error("free text has nothing to complete")
	}
}

func TestParseSpan(t *testing.T) {
	for in, want := range map[string]time.Duration{"30m": 30 * time.Minute, "6h": 6 * time.Hour, "7d": 7 * 24 * time.Hour} {
		if got, err := parseSpan(in); err != nil || got != want {
			t.Errorf("%s = %v, %v", in, got, err)
		}
	}
}

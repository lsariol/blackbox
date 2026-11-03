package lines

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const stampText = "2026-11-02T23:03:43.123456789Z"

func frame(stream byte, text string) []byte {
	head := make([]byte, 8)
	head[0] = stream
	binary.BigEndian.PutUint32(head[4:], uint32(len(text)))
	return append(head, text...)
}

func TestReaderSplitsFrames(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(frame(1, stampText+" first\n"))
	stream.Write(frame(2, stampText+" second\n"))
	stream.Write(frame(2, stampText+" \n"))
	stream.Write(frame(1, "no stamp here\n"))
	stream.Write(frame(1, stampText+" partial without a newline"))

	r := NewReader(&stream, false)
	want := []struct {
		stream int16
		text   string
		stamp  bool
	}{
		{Stdout, "first", true},
		{Stderr, "second", true},
		{Stderr, "", true},
		{Stdout, "no stamp here", false},
		{Stdout, "partial without a newline", true},
	}
	for i, w := range want {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if got.Stream != w.stream || string(got.Text) != w.text || got.Time.IsZero() == w.stamp {
			t.Errorf("line %d = %+v (%q)", i, got, got.Text)
		}
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want EOF", err)
	}
}

func TestReaderKeepsNanoseconds(t *testing.T) {
	r := NewReader(bytes.NewReader(frame(1, stampText+" x\n")), false)
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 2, 23, 3, 43, 123456789, time.UTC)
	if !got.Time.Equal(want) || got.Time.Nanosecond() != 123456789 {
		t.Errorf("time = %v", got.Time)
	}
}

func TestReaderCutOffStreamEnds(t *testing.T) {
	whole := frame(1, stampText+" x\n")
	for _, cut := range []int{3, 8, len(whole) - 1} {
		r := NewReader(bytes.NewReader(whole[:cut]), false)
		if _, err := r.Next(); !errors.Is(err, io.EOF) {
			t.Errorf("cut at %d: err = %v, want EOF", cut, err)
		}
	}
}

func TestReaderRefusesAnAbsurdFrame(t *testing.T) {
	head := make([]byte, 8)
	binary.BigEndian.PutUint32(head[4:], 1<<30)
	if _, err := NewReader(bytes.NewReader(head), false).Next(); err == nil || errors.Is(err, io.EOF) {
		t.Errorf("err = %v", err)
	}
}

func TestReaderTTY(t *testing.T) {
	stream := stampText + " first\r\n" + stampText + " second\n" + stampText + " last"
	r := NewReader(strings.NewReader(stream), true)
	for _, want := range []string{"first", "second", "last"} {
		got, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if string(got.Text) != want || got.Stream != Stdout || got.Time.IsZero() {
			t.Errorf("line = %+v (%q)", got, got.Text)
		}
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("err = %v", err)
	}
}

func TestReaderTTYLongLine(t *testing.T) {
	stream := stampText + " " + strings.Repeat("x", 500<<10) + "\n" + stampText + " after\n"
	r := NewReader(strings.NewReader(stream), true)
	first, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Text) < maxLine {
		t.Errorf("long line kept %d bytes", len(first.Text))
	}
	second, err := r.Next()
	if err != nil || string(second.Text) != "after" {
		t.Errorf("next = %q, %v", second.Text, err)
	}
}

func attrsOf(t *testing.T, rec Record) map[string]any {
	t.Helper()
	if rec.Attrs == nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Attrs, &m); err != nil {
		t.Fatalf("attrs %s: %v", rec.Attrs, err)
	}
	return m
}

func TestParseFormats(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		level   int16
		format  int16
		message string
		attrs   map[string]any
	}{
		{"plop JSON", `{"time":"2026-11-02T10:00:00Z","level":"WARN","msg":"delete payload dir","id":"a1","error":"gone"}`,
			LevelWarn, FormatJSON, "delete payload dir", map[string]any{"id": "a1", "error": "gone"}},
		{"JSON with severity and message", `{"severity":"error","message":"boom"}`, LevelError, FormatJSON, "boom", nil},
		{"JSON keeps a number", `{"level":"INFO","msg":"x","count":5}`, LevelInfo, FormatJSON, "x", map[string]any{"count": float64(5)}},
		{"JSON with no level", `{"a":1}`, LevelUnknown, FormatPlain, `{"a":1}`, nil},
		{"JSON with a numeric level", `{"level":30,"msg":"x"}`, LevelUnknown, FormatPlain, `{"level":30,"msg":"x"}`, nil},
		{"broken JSON", `{"level":"INFO","msg":`, LevelUnknown, FormatPlain, `{"level":"INFO","msg":`, nil},
		{"two JSON objects", `{"level":"INFO","msg":"x"} {"a":1}`, LevelUnknown, FormatPlain, `{"level":"INFO","msg":"x"} {"a":1}`, nil},

		{"slog text", `time=2026-11-02T10:00:00.000Z level=INFO msg="lookup done" operation=chat.send duration_ms=42`,
			LevelInfo, FormatSlogText, "lookup done", map[string]any{"operation": "chat.send", "duration_ms": "42"}},
		{"slog error", `time=2026-11-02T10:00:00Z level=ERROR msg=failed err="dial tcp: \"x\" refused"`,
			LevelError, FormatSlogText, "failed", map[string]any{"err": `dial tcp: "x" refused`}},
		{"slog offset", `time=x level=ERROR+2 msg=bad`, LevelError, FormatSlogText, "bad", nil},
		{"slog low offset", `time=x level=INFO-4 msg=low`, LevelDebug, FormatSlogText, "low", nil},
		{"slog with no level", `time=x msg=y`, LevelUnknown, FormatPlain, "time=x msg=y", nil},
		{"not really slog", `time=x level=INFO and then some words`, LevelUnknown, FormatPlain, "time=x level=INFO and then some words", nil},

		{"Lys plain", `2026/11/02 14:03:21 [Router] !gpt in "mobasity": replied 1.2s after the message`,
			LevelUnknown, FormatGoLog, `!gpt in "mobasity": replied 1.2s after the message`, map[string]any{"component": "Router"}},
		{"Lys error", `2026/11/02 14:03:21 ✗ [Enroll] refused`, LevelError, FormatGoLog, "refused", map[string]any{"component": "Enroll"}},
		{"Lys warning", `2026/11/02 14:03:21 ! [7TV] left pending`, LevelWarn, FormatGoLog, "left pending", map[string]any{"component": "7TV"}},
		{"Cove request", `2026/11/02 14:03:21 GET /v0/secrets/LYS_DATABASE_URL 200 1.2ms source=lys from=172.18.0.5`,
			LevelUnknown, FormatGoLog, `GET /v0/secrets/LYS_DATABASE_URL 200 1.2ms source=lys from=172.18.0.5`, nil},
		{"micro Go log", `2026/11/02 14:03:21.123456 hello`, LevelUnknown, FormatGoLog, "hello", nil},

		{"cloudflared", `2026-11-02T23:03:43Z INF Registered tunnel connection connIndex=0`,
			LevelInfo, FormatCloudflared, "Registered tunnel connection connIndex=0", nil},
		{"cloudflared error", `2026-11-02T23:03:43Z ERR failed to serve`, LevelError, FormatCloudflared, "failed to serve", nil},
		{"cloudflared warning", `2026-11-02T23:03:43Z WRN slow`, LevelWarn, FormatCloudflared, "slow", nil},

		{"Postgres log", `2026-11-02 23:03:43.123 UTC [27] LOG:  checkpoint starting: time`, LevelInfo, FormatPostgres, "checkpoint starting: time", nil},
		{"Postgres fatal", `2026-11-02 23:03:43.123 UTC [27] FATAL:  the database system is starting up`,
			LevelError, FormatPostgres, "the database system is starting up", nil},
		{"Postgres warning", `2026-11-02 23:03:43.123 UTC [27] WARNING:  odd`, LevelWarn, FormatPostgres, "odd", nil},
		{"Postgres detail", `2026-11-02 23:03:43.123 UTC [27] DETAIL:  more`, LevelUnknown, FormatPlain, `2026-11-02 23:03:43.123 UTC [27] DETAIL:  more`, nil},

		{"panic", `panic: runtime error: index out of range`, LevelError, FormatPlain, `panic: runtime error: index out of range`, nil},
		{"fatal error", `fatal error: concurrent map writes`, LevelError, FormatPlain, `fatal error: concurrent map writes`, nil},
		{"stack line", `goroutine 1 [running]:`, LevelUnknown, FormatPlain, `goroutine 1 [running]:`, nil},
		{"plain", `anything at all`, LevelUnknown, FormatPlain, `anything at all`, nil},
		{"empty", ``, LevelUnknown, FormatPlain, ``, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := Parse([]byte(tt.line))
			if rec.Level != tt.level || rec.Format != tt.format || rec.Message != tt.message {
				t.Errorf("got level %d, format %d, message %q", rec.Level, rec.Format, rec.Message)
			}
			got := attrsOf(t, rec)
			if len(got) != len(tt.attrs) {
				t.Fatalf("attrs = %v, want %v", got, tt.attrs)
			}
			for k, v := range tt.attrs {
				if got[k] != v {
					t.Errorf("attrs[%s] = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestParseMakesTextStorable(t *testing.T) {
	rec := Parse([]byte("bad \xff byte and a \x00 NUL"))
	if strings.ContainsRune(rec.Message, 0) || !utf8.ValidString(rec.Message) {
		t.Errorf("message = %q", rec.Message)
	}

	long := Parse(append(bytes.Repeat([]byte("é"), maxLine), []byte("end")...))
	if !long.Truncated || len(long.Message) > maxLine || !utf8.ValidString(long.Message) {
		t.Errorf("truncated = %v, %d bytes", long.Truncated, len(long.Message))
	}

	nul := Parse([]byte(`{"level":"INFO","msg":"a\u0000b","k\u0000ey":"v\u0000","big":1e999999,"list":["\u0000"]}`))
	if bytes.Contains(nul.Attrs, []byte(`\u0000`)) || strings.ContainsRune(nul.Message, 0) {
		t.Errorf("NUL survived: %q, %s", nul.Message, nul.Attrs)
	}
	if !bytes.Contains(nul.Attrs, []byte(`"1e999999"`)) {
		t.Errorf("a number Postgres can't hold must become text: %s", nul.Attrs)
	}
}

func TestRedaction(t *testing.T) {
	const value = "hunter2hunter2"
	tests := []struct {
		name string
		line string
	}{
		{"bearer", "calling with Authorisation Bearer " + value},
		{"lowercase bearer", "bearer " + value},
		{"database URL", "connect postgres://blackbox_app:" + value + "@sparkdb:5432/blackbox_db failed"},
		{"authorization header", "Authorization: Basic " + value},
		{"cookie header", "Set-Cookie: session=" + value},
		{"request cookie", "Cookie: a=1; session=" + value},
		{"cookie setting", "cookie=" + value},
		{"cookie in JSON text", `sent {"cookie":"` + value + `"}`},
		{"token", "token=" + value + " more"},
		{"access token", "access_token=" + value + "&x=1"},
		{"password with a colon", `password: "` + value + `"`},
		{"client secret", "client_secret=" + value},
		{"api key", "api-key: " + value},
		{"JSON text", `failed {"password":"` + value + `"}`},
		{"JSON field", `{"level":"INFO","msg":"x","password":"` + value + `"}`},
		{"nested JSON field", `{"level":"INFO","msg":"x","req":{"headers":{"cookie":"` + value + `"}}}`},
		{"URL in a JSON field", `{"level":"INFO","msg":"x","url":"https://u:` + value + `@host/"}`},
		{"list in a JSON field", `{"level":"INFO","msg":"x","list":["token=` + value + `"]}`},
		{"slog field", `time=x level=INFO msg=y token=` + value},
		{"slog error text", `time=x level=ERROR msg=y err="bad password=` + value + `"`},
		{"message text", `{"level":"INFO","msg":"token=` + value + `"}`},
		{"Go log", `2026/11/02 14:03:21 [Twitch] token=` + value},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := Parse([]byte(tt.line))
			if strings.Contains(rec.Message+string(rec.Attrs), value) {
				t.Errorf("value survived: %q %s", rec.Message, rec.Attrs)
			}
			if !strings.Contains(rec.Message+string(rec.Attrs), redacted) {
				t.Errorf("no marker: %q %s", rec.Message, rec.Attrs)
			}
			if !rec.Redacted {
				t.Error("Redacted should be set")
			}
		})
	}
}

func TestRedactionLeavesOrdinaryTextAlone(t *testing.T) {
	for _, line := range []string{
		"GET /v0/secrets/LYS_TOKEN 200 1.2ms source=lys from=172.18.0.5",
		"tokens: record last use failed",
		"https://example.com/path?page=2",
		"http://sonar-api:2500/v1/status",
		"the password was wrong",
		"bearer",
		"[2026-11-03 00:51:13.72] VulpesHD  : @Supibot    - onreeee, Your daily cookie: Sugar cookie, enjoy!",
		"Your received cookie: a cookie from a friend",
	} {
		rec := Parse([]byte(line))
		if rec.Message != line || rec.Redacted {
			t.Errorf("%q became %q", line, rec.Message)
		}
	}
}

func TestRedactionDoesNotCountBlackboxlogMarkers(t *testing.T) {
	rec := Parse([]byte(`{"level":"INFO","msg":"x","password":"[redacted]","url":"token=[redacted]"}`))
	if rec.Redacted {
		t.Errorf("nothing new was hidden: %s", rec.Attrs)
	}
}

func TestLevelNames(t *testing.T) {
	for _, name := range []string{"debug", "info", "warn", "error"} {
		level, ok := LevelFromName(name)
		if !ok || LevelName(level) != name {
			t.Errorf("%s: %d, %v", name, level, ok)
		}
	}
	if _, ok := LevelFromName("unknown"); ok {
		t.Error("unknown isn't a level to filter by")
	}
	if StreamName(Stderr) != "stderr" || StreamName(Stdout) != "stdout" {
		t.Error("stream names")
	}
}

func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		`{"level":"INFO","msg":"x"}`, `time=x level=INFO msg="a`, "2026/11/02 14:03:21 ✗ [a] b", "panic: x",
		"\x00\xff", `{"level":"ERROR+999999999999999999999","msg":"x"}`, `level=x= msg="`, "token=",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		rec := Parse(line)
		if strings.ContainsRune(rec.Message, 0) || !utf8.ValidString(rec.Message) {
			t.Fatalf("message isn't storable: %q", rec.Message)
		}
		if rec.Attrs != nil && (!json.Valid(rec.Attrs) || bytes.Contains(rec.Attrs, []byte(`\u0000`))) {
			t.Fatalf("attrs aren't storable: %s", rec.Attrs)
		}
	})
}

func FuzzReader(f *testing.F) {
	f.Add(frame(1, stampText+" x\n"), false)
	f.Add([]byte(stampText+" x\n"), true)
	f.Fuzz(func(t *testing.T, stream []byte, tty bool) {
		r := NewReader(bytes.NewReader(stream), tty)
		for range 1000 {
			if _, err := r.Next(); err != nil {
				return
			}
		}
	})
}

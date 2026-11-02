package proxy

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsariol/blackbox/internal/containers"
)

var (
	idA = strings.Repeat("a", 64)
	idB = strings.Repeat("b", 64)
	idC = strings.Repeat("c", 64)
)

// inspectJSON is Docker's answer for idA, including what must never get out.
const inspectJSON = `{
	"Id": "` + "%ID%" + `",
	"Name": "/lys",
	"Created": "2026-11-01T10:00:00.5Z",
	"State": {"Status": "running", "StartedAt": "2026-11-01T10:00:01Z", "FinishedAt": "0001-01-01T00:00:00Z"},
	"HostConfig": {"LogConfig": {"Type": "json-file", "Config": {"secret-opt": "HIDDEN-LOGOPT"}}, "Binds": ["/HIDDEN-MOUNT:/x"]},
	"Mounts": [{"Source": "/HIDDEN-MOUNT"}],
	"Config": {
		"Image": "lys:latest", "Tty": %TTY%,
		"Env": ["LYS_TWITCH_TOKEN=HIDDEN-ENV"], "Cmd": ["/lys", "--password=HIDDEN-CMD"],
		"Labels": {
			"com.docker.compose.project": "lys", "com.docker.compose.service": "bot",
			"com.mobasity.version": "v0.3.0", "com.mobasity.logs": "%LOGS%",
			"traefik.password": "HIDDEN-LABEL"
		}
	}
}`

// fakeDocker answers like Docker's API, recording every path it's asked for.
type fakeDocker struct {
	mu    sync.Mutex
	paths []string

	logsLabel string
	tty       bool
	logBody   []byte
	follow    chan struct{} // while set, a log stream stays open until closed
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.RequestURI())
	f.mu.Unlock()

	path := r.URL.Path
	switch {
	case path == "/_ping":
		io.WriteString(w, "OK")
	case path == "/containers/json":
		json.NewEncoder(w).Encode([]map[string]any{
			{"Id": idA, "Names": []string{"/lys"}},
			{"Id": idB, "Names": []string{"/broken"}},
			{"Id": idC, "Names": []string{"/gone"}},
		})
	case strings.HasPrefix(path, "/containers/"+idB):
		w.WriteHeader(http.StatusInternalServerError)
	case strings.HasPrefix(path, "/containers/"+idC):
		w.WriteHeader(http.StatusNotFound)
	case strings.HasSuffix(path, "/json"):
		tty := "false"
		if f.tty {
			tty = "true"
		}
		body := strings.NewReplacer("%ID%", idA, "%TTY%", tty, "%LOGS%", f.logsLabel).Replace(inspectJSON)
		io.WriteString(w, body)
	case strings.HasSuffix(path, "/logs"):
		w.Write(f.logBody)
		w.(http.Flusher).Flush()
		if f.follow != nil && r.URL.Query().Get("follow") == "1" {
			select {
			case <-f.follow:
			case <-r.Context().Done():
			}
		}
	default:
		t := http.StatusNotFound
		w.WriteHeader(t)
	}
}

func (f *fakeDocker) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func newTestServer(t *testing.T, f *fakeDocker) *httptest.Server {
	t.Helper()
	dockerSrv := httptest.NewServer(f)
	t.Cleanup(dockerSrv.Close)
	s := &Server{
		docker: &docker{base: dockerSrv.URL, http: dockerSrv.Client()},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		slots:  make(chan struct{}, maxStreams),
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// frame wraps text in Docker's stream header.
func frame(stream byte, text string) []byte {
	head := make([]byte, 8)
	head[0] = stream
	binary.BigEndian.PutUint32(head[4:], uint32(len(text)))
	return append(head, text...)
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestContainersCopiesOnlyWhatItNeeds(t *testing.T) {
	srv := newTestServer(t, &fakeDocker{})
	status, body := get(t, srv.URL+"/v1/containers")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if strings.Contains(body, "HIDDEN") {
		t.Fatalf("something private got out: %s", body)
	}

	var answer containers.ContainersAnswer
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatal(err)
	}
	if len(answer.Containers) != 1 {
		t.Fatalf("containers = %+v", answer.Containers)
	}
	c := answer.Containers[0]
	if c.ID != idA || c.Name != "lys" || c.Project != "lys" || c.Service != "bot" || c.Image != "lys:latest" ||
		c.State != "running" || c.LogDriver != "json-file" || c.Version != "v0.3.0" || c.TTY || c.Excluded {
		t.Errorf("container = %+v", c)
	}
	if c.StartedAt == nil || c.FinishedAt != nil || c.CreatedAt.IsZero() {
		t.Errorf("times = %+v", c)
	}
	if len(answer.Failed) != 1 || answer.Failed[0].ID != idB {
		t.Errorf("failed = %+v", answer.Failed)
	}
}

func TestContainersMarksOptedOut(t *testing.T) {
	srv := newTestServer(t, &fakeDocker{logsLabel: "off"})
	_, body := get(t, srv.URL+"/v1/containers")
	var answer containers.ContainersAnswer
	json.Unmarshal([]byte(body), &answer)
	if !answer.Containers[0].Excluded {
		t.Error("the container should be marked excluded")
	}
}

func TestLogsBuildsTheDockerCall(t *testing.T) {
	tests := []struct {
		query string
		want  string
	}{
		{"since=1793000000123456789", "/containers/" + idA + "/logs?stdout=1&stderr=1&timestamps=1&follow=1&since=1793000000.123456789"},
		{"since=5", "/containers/" + idA + "/logs?stdout=1&stderr=1&timestamps=1&follow=1&since=0.000000005"},
		{"from=end", "/containers/" + idA + "/logs?stdout=1&stderr=1&timestamps=1&follow=1&tail=0"},
	}
	for _, tt := range tests {
		f := &fakeDocker{logBody: []byte("raw bytes")}
		srv := newTestServer(t, f)
		status, body := get(t, srv.URL+"/v1/logs/"+idA+"?"+tt.query)
		if status != http.StatusOK || body != "raw bytes" {
			t.Errorf("%s: status %d, body %q", tt.query, status, body)
		}
		paths := f.seen()
		if got := paths[len(paths)-1]; got != tt.want {
			t.Errorf("%s: Docker was asked for\n%s, want\n%s", tt.query, got, tt.want)
		}
	}
}

func TestLogsRefusesBadRequests(t *testing.T) {
	tests := []struct {
		name string
		path string
		want int
	}{
		{"short id", "/v1/logs/abc?since=1", http.StatusBadRequest},
		{"uppercase id", "/v1/logs/" + strings.Repeat("A", 64) + "?since=1", http.StatusBadRequest},
		{"no parameter", "/v1/logs/" + idA, http.StatusBadRequest},
		{"letters in since", "/v1/logs/" + idA + "?since=abc", http.StatusBadRequest},
		{"too long since", "/v1/logs/" + idA + "?since=" + strings.Repeat("9", 20), http.StatusBadRequest},
		{"overflowing since", "/v1/logs/" + idA + "?since=9999999999999999999", http.StatusBadRequest},
		{"negative since", "/v1/logs/" + idA + "?since=-1", http.StatusBadRequest},
		{"two sinces", "/v1/logs/" + idA + "?since=1&since=2", http.StatusBadRequest},
		{"injected parameter", "/v1/logs/" + idA + "?since=1%26tail=all", http.StatusBadRequest},
		{"both", "/v1/logs/" + idA + "?since=1&from=end", http.StatusBadRequest},
		{"extra parameter", "/v1/logs/" + idA + "?since=1&tail=all", http.StatusBadRequest},
		{"other from", "/v1/logs/" + idA + "?from=start", http.StatusBadRequest},
		{"other path", "/v1/exec", http.StatusNotFound},
		{"container list with a parameter", "/v1/containers?all=1", http.StatusBadRequest},
		{"latest with a parameter", "/v1/logs/" + idA + "/latest?tail=all", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeDocker{}
			srv := newTestServer(t, f)
			status, _ := get(t, srv.URL+tt.path)
			if status != tt.want {
				t.Errorf("status = %d, want %d", status, tt.want)
			}
			if tt.want != http.StatusOK {
				if paths := f.seen(); len(paths) != 0 {
					t.Errorf("Docker was called: %v", paths)
				}
			}
		})
	}
}

func TestLogsOnlyAnswersGET(t *testing.T) {
	srv := newTestServer(t, &fakeDocker{})
	resp, err := http.Post(srv.URL+"/v1/logs/"+idA+"?since=1", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestOptedOutContainerIsRefused(t *testing.T) {
	f := &fakeDocker{logsLabel: "off", logBody: []byte("secret lines")}
	srv := newTestServer(t, f)
	for _, path := range []string{"/v1/logs/" + idA + "?since=1", "/v1/logs/" + idA + "/latest"} {
		status, body := get(t, srv.URL+path)
		if status != http.StatusForbidden || strings.Contains(body, "secret lines") {
			t.Errorf("%s: status %d, body %s", path, status, body)
		}
	}
	for _, p := range f.seen() {
		if strings.Contains(p, "/logs") {
			t.Errorf("Docker's logs were read: %s", p)
		}
	}
}

func TestMissingContainerIs404(t *testing.T) {
	srv := newTestServer(t, &fakeDocker{})
	status, _ := get(t, srv.URL+"/v1/logs/"+idC+"?since=1")
	if status != http.StatusNotFound {
		t.Errorf("status = %d", status)
	}
}

func TestStreamsAreLimited(t *testing.T) {
	f := &fakeDocker{logBody: []byte("x"), follow: make(chan struct{})}
	defer close(f.follow)
	srv := newTestServer(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range maxStreams {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/logs/"+idA+"?since=1", nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		buf := make([]byte, 1)
		io.ReadFull(resp.Body, buf)
	}
	status, _ := get(t, srv.URL+"/v1/logs/"+idA+"?since=1")
	if status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", status)
	}
}

func TestLatest(t *testing.T) {
	const when = "2026-11-02T23:03:43.123456789Z"
	long := when + " " + strings.Repeat("private text ", 100) + "\n"
	tests := []struct {
		name string
		tty  bool
		body []byte
		want string
	}{
		{"framed", false, frame(2, long), when},
		{"tty", true, []byte(long), when},
		{"nothing written", false, nil, ""},
		{"unreadable", false, frame(1, "garbage without a stamp\n"), ""},
		{"too short", false, []byte{1, 0}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeDocker{tty: tt.tty, logBody: tt.body}
			srv := newTestServer(t, f)
			status, body := get(t, srv.URL+"/v1/logs/"+idA+"/latest")
			if status != http.StatusOK {
				t.Fatalf("status %d: %s", status, body)
			}
			if strings.Contains(body, "private") {
				t.Fatalf("log text got out: %s", body)
			}
			var answer containers.LatestAnswer
			if err := json.Unmarshal([]byte(body), &answer); err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if answer.Latest != nil {
					t.Errorf("latest = %v, want none", answer.Latest)
				}
				return
			}
			want, _ := time.Parse(time.RFC3339Nano, tt.want)
			if answer.Latest == nil || !answer.Latest.Equal(want) {
				t.Errorf("latest = %v, want %v", answer.Latest, want)
			}
			paths := f.seen()
			if got := paths[len(paths)-1]; !strings.HasSuffix(got, "&tail=1") {
				t.Errorf("Docker call = %s", got)
			}
		})
	}
}

func TestHealth(t *testing.T) {
	srv := newTestServer(t, &fakeDocker{})
	status, body := get(t, srv.URL+"/healthz")
	if status != http.StatusOK || !strings.Contains(body, "true") {
		t.Errorf("status %d: %s", status, body)
	}
}

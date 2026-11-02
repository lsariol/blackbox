// Package proxy is blackbox-proxy, the only part of Blackbox with the Docker
// socket. It answers fixed requests on a Unix socket and builds every Docker
// call itself. It never parses log text: that can be hostile, and this is the
// one process with root-equivalent access, so log bytes pass through unread.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/lsariol/blackbox/internal/containers"
)

const (
	// DockerSocket is where the compose file mounts Docker's socket.
	DockerSocket = "/var/run/docker.sock"

	// parallel is how many inspect calls the proxy makes at once.
	parallel = 8

	// maxStreams is how many log streams may be open at once.
	maxStreams = 32

	// latestTimeout bounds reading one container's newest line.
	latestTimeout = 5 * time.Second

	// stampBytes is enough of a line to hold Docker's frame header and timestamp.
	stampBytes = 8 + 40

	// frameHeader is the size of Docker's header on a stream without a TTY.
	frameHeader = 8
)

// sincePattern is Unix nanoseconds, the only value `since` accepts.
var sincePattern = regexp.MustCompile(`^[0-9]{1,19}$`)

// Server answers the collector's requests.
type Server struct {
	docker *docker
	log    *slog.Logger
	slots  chan struct{} // one per open log stream
}

// New returns a server that reads Docker through dockerSocket.
func New(dockerSocket string, log *slog.Logger) *Server {
	return &Server{docker: newDocker(dockerSocket), log: log, slots: make(chan struct{}, maxStreams)}
}

// Handler routes the requests; any other path is 404, any other method 405.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/containers", s.containers)
	mux.HandleFunc("GET /v1/logs/{id}", s.logs)
	mux.HandleFunc("GET /v1/logs/{id}/latest", s.latest)
	mux.HandleFunc("GET /healthz", s.health)
	return mux
}

// Serve answers on a Unix socket at path until ctx is cancelled. A socket
// left by an earlier run is replaced.
func (s *Server) Serve(ctx context.Context, path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("proxy: removing the old socket %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("proxy: listening on %s: %w: is the proxy volume mounted at its folder?", path, err)
	}
	defer os.Remove(path)
	// Only the proxy and the collector mount the folder, so anyone who can see
	// the socket may use it; the collector runs as another user.
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return fmt.Errorf("proxy: making %s usable by the collector: %w", path, err)
	}

	// No read or write timeout: a log stream lasts as long as its container,
	// and a read timeout would cancel its context. Requests have no body, and
	// each Docker call has its own limit.
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
		srv.Close()
	}()

	s.log.Info("proxy listening", "socket", path)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("proxy: %w", err)
	}
	return nil
}

func (s *Server) containers(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Query()) != 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "this request takes no parameters"})
		return
	}
	list, err := s.docker.list(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}

	found := make([]*containers.Container, len(list))
	errs := make([]error, len(list))
	each(len(list), func(i int) {
		c, err := s.docker.inspect(r.Context(), list[i].ID)
		switch {
		case errors.Is(err, errNotFound):
			// Removed since the list: it no longer exists to report.
		case err != nil:
			errs[i] = err
		default:
			out := toContainer(c)
			found[i] = &out
		}
	})

	// One container Docker can't inspect mustn't hide all the others.
	answer := containers.ContainersAnswer{Containers: []containers.Container{}, Failed: []containers.ContainerError{}}
	for i, c := range found {
		switch {
		case c != nil:
			answer.Containers = append(answer.Containers, *c)
		case errs[i] != nil:
			s.log.Warn("Docker couldn't inspect a container", "id", list[i].ID, "err", errs[i])
			answer.Failed = append(answer.Failed, containers.ContainerError{ID: list[i].ID, Name: list[i].name(), Reason: errs[i].Error()})
		}
	}
	writeJSON(w, http.StatusOK, answer)
}

// logs streams one container's output, unchanged, until it ends or the caller
// hangs up.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	query, reason := dockerQuery(r.URL.Query())
	if !idPattern.MatchString(id) {
		reason = "id must be a full container ID"
	}
	if reason != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": reason})
		return
	}

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": fmt.Sprintf("at most %d log streams may be open", maxStreams)})
		return
	}

	if !s.allowed(w, r, id) {
		return
	}
	body, err := s.docker.logs(r.Context(), id, query)
	if err != nil {
		s.failLogs(w, r, err)
		return
	}
	defer body.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	flusher := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
			if err := flusher.Flush(); err != nil {
				return
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) && r.Context().Err() == nil {
				s.log.Warn("Docker log stream ended early", "id", id, "err", readErr)
			}
			return
		}
	}
}

// dockerQuery checks a log request's parameters and builds the Docker query
// from them; reason says what's wrong.
func dockerQuery(params url.Values) (query, reason string) {
	const want = "give since=<unix nanoseconds> or from=end, and nothing else"
	if len(params) != 1 {
		return "", want
	}
	if v, ok := params["since"]; ok {
		if len(v) != 1 || !sincePattern.MatchString(v[0]) {
			return "", want
		}
		ns, err := strconv.ParseInt(v[0], 10, 64)
		if err != nil {
			return "", want
		}
		return fmt.Sprintf("follow=1&since=%d.%09d", ns/1e9, ns%1e9), ""
	}
	if v, ok := params["from"]; ok && len(v) == 1 && v[0] == "end" {
		return "follow=1&tail=0", ""
	}
	return "", want
}

// allowed checks that the container exists and hasn't opted out, answering
// the request when it hasn't.
func (s *Server) allowed(w http.ResponseWriter, r *http.Request, id string) bool {
	c, err := s.docker.inspect(r.Context(), id)
	if err != nil {
		s.failLogs(w, r, err)
		return false
	}
	if excluded(c) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this container has com.mobasity.logs=off"})
		return false
	}
	return true
}

// latest answers with the time of the container's newest line, and never its
// text. Reading from the end works past a damaged part of a log.
func (s *Server) latest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) || len(r.URL.Query()) != 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be a full container ID, with no parameters"})
		return
	}
	c, err := s.docker.inspect(r.Context(), id)
	if err != nil {
		s.failLogs(w, r, err)
		return
	}
	if excluded(c) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this container has com.mobasity.logs=off"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), latestTimeout)
	defer cancel()
	body, err := s.docker.logs(ctx, id, "tail=1")
	if err != nil {
		s.failLogs(w, r, err)
		return
	}
	defer body.Close()

	head := make([]byte, stampBytes)
	n, err := io.ReadFull(body, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		s.fail(w, fmt.Errorf("Docker: reading the newest line: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, containers.LatestAnswer{Latest: stamp(head[:n], c.Config.Tty)})
}

// stamp reads the timestamp Docker put in front of a line: the first word
// after the frame header, if there is one. It looks at nothing else.
func stamp(head []byte, tty bool) *time.Time {
	if !tty {
		if len(head) < frameHeader {
			return nil
		}
		head = head[frameHeader:]
	}
	word, _, ok := bytes.Cut(head, []byte(" "))
	if !ok {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, string(word))
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.docker.ping(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// failLogs answers a failed call about one container.
func (s *Server) failLogs(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such container"})
		return
	}
	if r.Context().Err() != nil {
		return
	}
	s.fail(w, err)
}

// fail logs err and answers 502 with it.
func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("Docker call failed", "err", err)
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// each runs fn for 0..n-1, at most parallel at a time, and waits for all.
func each(n int, fn func(i int)) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, parallel)
	for i := range n {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			fn(i)
		}()
	}
	wg.Wait()
}

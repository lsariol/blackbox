package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/lsariol/blackbox/internal/containers"
)

const (
	// maxDockerAnswer is the largest JSON answer read from Docker for one call.
	maxDockerAnswer = 8 << 20

	// callTimeout bounds one Docker call other than a log stream.
	callTimeout = 10 * time.Second
)

// errNotFound is Docker's 404: the container went away between two calls.
var errNotFound = errors.New("not found")

// idPattern is a full container ID, checked before one goes into a path.
var idPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// docker is a minimal Docker Engine API client over its socket: the read calls
// the proxy makes, and nothing else.
type docker struct {
	base string
	http *http.Client
}

func newDocker(socket string) *docker {
	return &docker{
		base: "http://docker",
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
				MaxIdleConnsPerHost: parallel,
			},
		},
	}
}

// listed is one container from the list: enough to pick what to inspect.
type listed struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
}

// name is the container's name without Docker's leading slash.
func (l listed) name() string {
	if len(l.Names) == 0 {
		return ""
	}
	return strings.TrimPrefix(l.Names[0], "/")
}

// inspected holds the inspect fields Blackbox uses. Fields not declared here,
// such as Config.Env, Config.Cmd and Mounts, are never read into memory.
type inspected struct {
	ID      string `json:"Id"`
	Name    string `json:"Name"`
	Created string `json:"Created"`
	State   struct {
		Status     string `json:"Status"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`
	HostConfig struct {
		LogConfig struct {
			Type string `json:"Type"`
		} `json:"LogConfig"`
	} `json:"HostConfig"`
	Config struct {
		Image  string            `json:"Image"`
		Tty    bool              `json:"Tty"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func (d *docker) ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return d.get(ctx, "/_ping", nil)
}

// list returns every container, stopped ones included.
func (d *docker) list(ctx context.Context) ([]listed, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var all []listed
	if err := d.get(ctx, "/containers/json?all=1", &all); err != nil {
		return nil, err
	}
	for _, c := range all {
		if !idPattern.MatchString(c.ID) {
			return nil, fmt.Errorf("Docker: listing containers: unexpected container ID %q", c.ID)
		}
	}
	return all, nil
}

func (d *docker) inspect(ctx context.Context, id string) (inspected, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var c inspected
	err := d.get(ctx, "/containers/"+id+"/json", &c)
	return c, err
}

// logs opens a container's log output. query holds fixed values and numbers
// the handler checked; nothing a caller sent is copied in.
func (d *docker) logs(ctx context.Context, id, query string) (io.ReadCloser, error) {
	path := "/containers/" + id + "/logs?stdout=1&stderr=1&timestamps=1&" + query
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Docker: %w: is the Docker socket mounted at %s?", err, DockerSocket)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, errNotFound
	}
	defer resp.Body.Close()
	var e struct {
		Message string `json:"message"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&e)
	return nil, fmt.Errorf("Docker: GET %s: %s: %s", route(path), resp.Status, e.Message)
}

// get fetches path and decodes the JSON answer into v (nil: discard it).
func (d *docker) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("Docker: %w: is the Docker socket mounted at %s?", err, DockerSocket)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("Docker: GET %s: %s", route(path), resp.Status)
	}
	body := io.LimitReader(resp.Body, maxDockerAnswer)
	if v == nil {
		_, err := io.Copy(io.Discard, body)
		return err
	}
	if err := json.NewDecoder(body).Decode(v); err != nil {
		return fmt.Errorf("Docker: GET %s: unreadable answer: %w", route(path), err)
	}
	return nil
}

// route is path without its query, for error messages.
func route(path string) string {
	route, _, _ := strings.Cut(path, "?")
	return route
}

// toContainer copies the fields Blackbox uses, one by one.
func toContainer(c inspected) containers.Container {
	labels := c.Config.Labels
	name := strings.TrimPrefix(c.Name, "/")

	out := containers.Container{
		ID:         c.ID,
		Name:       name,
		Project:    labels[containers.ProjectLabel],
		Service:    labels[containers.ServiceLabel],
		Image:      c.Config.Image,
		State:      c.State.Status,
		TTY:        c.Config.Tty,
		LogDriver:  c.HostConfig.LogConfig.Type,
		Version:    labels[containers.VersionLabel],
		Excluded:   excluded(c),
		CreatedAt:  parseTime(c.Created),
		StartedAt:  optionalTime(c.State.StartedAt),
		FinishedAt: optionalTime(c.State.FinishedAt),
	}
	if out.Project == "" {
		out.Service = name
	}
	return out
}

// excluded reports whether the container opted out of collection.
func excluded(c inspected) bool {
	return strings.EqualFold(c.Config.Labels[containers.LogsLabel], containers.LogsOff)
}

// parseTime reads one of Docker's timestamps; Docker's zero time and
// anything unreadable become the zero time.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return time.Time{}
	}
	return t.UTC()
}

func optionalTime(s string) *time.Time {
	t := parseTime(s)
	if t.IsZero() {
		return nil
	}
	return &t
}

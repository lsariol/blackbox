package containers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	// maxAnswer is the largest JSON answer the client reads from the proxy.
	maxAnswer = 4 << 20

	// callTimeout bounds every call except a log stream.
	callTimeout = 15 * time.Second
)

var (
	// ErrBusy means the proxy is at its limit of open log streams.
	ErrBusy = errors.New("the proxy is at its limit of open log streams")
	// ErrGone means the container no longer exists.
	ErrGone = errors.New("the container no longer exists")
)

// Client talks to blackbox-proxy over its socket.
type Client struct {
	base   string
	socket string
	http   *http.Client
}

// NewClient returns a client for the proxy listening on socket.
func NewClient(socket string) *Client {
	return &Client{
		base:   "http://blackbox-proxy",
		socket: socket,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
		},
	}
}

// Containers returns every container, stopped ones included, and the ones
// Docker couldn't inspect.
func (c *Client) Containers(ctx context.Context) (ContainersAnswer, error) {
	var answer ContainersAnswer
	err := c.get(ctx, "/v1/containers", &answer)
	return answer, err
}

// Latest returns the time of the container's newest line; ok is false when it
// has written nothing.
func (c *Client) Latest(ctx context.Context, id string) (latest time.Time, ok bool, err error) {
	var answer LatestAnswer
	if err := c.get(ctx, "/v1/logs/"+url.PathEscape(id)+"/latest", &answer); err != nil {
		return time.Time{}, false, err
	}
	if answer.Latest == nil {
		return time.Time{}, false, nil
	}
	return *answer.Latest, true, nil
}

// Healthy returns nil when the proxy answers and can reach Docker.
func (c *Client) Healthy(ctx context.Context) error {
	return c.get(ctx, "/healthz", nil)
}

// Stream opens a container's log stream: Docker's bytes, unchanged, until the
// container stops or ctx is cancelled.
func (c *Client) Stream(ctx context.Context, id string, start Start) (io.ReadCloser, error) {
	query := "since=" + strconv.FormatInt(start.SinceNanos, 10)
	if start.FromEnd {
		query = "from=end"
	}
	path := "/v1/logs/" + url.PathEscape(id)
	resp, err := c.do(ctx, path+"?"+query)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, statusError(path, resp)
	}
	return resp.Body, nil
}

func (c *Client) do(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("proxy: %w: is blackbox-proxy running, with its socket at %s?", err, c.socket)
	}
	return resp, nil
}

func (c *Client) get(ctx context.Context, path string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	resp, err := c.do(ctx, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return statusError(path, resp)
	}
	if v == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return fmt.Errorf("proxy: GET %s: %w", path, err)
	}
	if len(data) > maxAnswer {
		return fmt.Errorf("proxy: GET %s: answer over %d bytes, refused", path, maxAnswer)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("proxy: GET %s: unreadable answer: %w", path, err)
	}
	return nil
}

// statusError describes an answer other than 200.
func statusError(path string, resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return ErrBusy
	case http.StatusNotFound:
		return ErrGone
	}
	var e struct {
		Error string `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&e)
	if e.Error == "" {
		e.Error = resp.Status
	}
	return fmt.Errorf("proxy: GET %s: %s", path, e.Error)
}

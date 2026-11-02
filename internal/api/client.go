package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// maxAnswer is the largest answer the client reads.
const maxAnswer = 64 << 20

// clientTimeout is a little over the server's own limit.
const clientTimeout = 10 * time.Second

// Client calls the API, as the CLI does.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient returns a client for the API at base.
func NewClient(base, token string) *Client {
	return &Client{base: base, token: token, http: &http.Client{Timeout: clientTimeout}}
}

// Logs runs a search; query holds the parameters of GET /v1/logs.
func (c *Client) Logs(ctx context.Context, query url.Values) (LogsAnswer, error) {
	var answer LogsAnswer
	err := c.get(ctx, "/v1/logs", query, &answer)
	return answer, err
}

// Record returns one record.
func (c *Client) Record(ctx context.Context, id int64) (Record, error) {
	var answer Record
	err := c.get(ctx, "/v1/logs/"+strconv.FormatInt(id, 10), nil, &answer)
	return answer, err
}

// Context returns the lines around a record.
func (c *Client) Context(ctx context.Context, id int64, before, after int) (ContextAnswer, error) {
	var answer ContextAnswer
	query := url.Values{"before": {strconv.Itoa(before)}, "after": {strconv.Itoa(after)}}
	err := c.get(ctx, "/v1/logs/"+strconv.FormatInt(id, 10)+"/context", query, &answer)
	return answer, err
}

// Sources lists what's stored.
func (c *Client) Sources(ctx context.Context) (SourcesAnswer, error) {
	var answer SourcesAnswer
	err := c.get(ctx, "/v1/sources", nil, &answer)
	return answer, err
}

// Gaps lists the gaps noted since from.
func (c *Client) Gaps(ctx context.Context, from time.Time) (GapsAnswer, error) {
	var answer GapsAnswer
	err := c.get(ctx, "/v1/gaps", url.Values{"from": {from.UTC().Format(time.RFC3339)}}, &answer)
	return answer, err
}

// Status returns collection health.
func (c *Client) Status(ctx context.Context) (StatusAnswer, error) {
	var answer StatusAnswer
	err := c.get(ctx, "/v1/status", nil, &answer)
	return answer, err
}

// Migrations lists this build's migrations.
func (c *Client) Migrations(ctx context.Context) (MigrationsAnswer, error) {
	var answer MigrationsAnswer
	err := c.get(ctx, "/v1/migrations", nil, &answer)
	return answer, err
}

func (c *Client) get(ctx context.Context, path string, query url.Values, v any) error {
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("API: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("API: can't reach blackbox-api: %w: run the CLI in its container (docker exec -it blackbox-api /blackbox shell)", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return fmt.Errorf("API: reading the answer: %w", err)
	}
	if len(data) > maxAnswer {
		return fmt.Errorf("API: the answer is over %d bytes: ask for fewer lines", maxAnswer)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) != nil || e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("API: %s", e.Error)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("API: unreadable answer: %w", err)
	}
	return nil
}

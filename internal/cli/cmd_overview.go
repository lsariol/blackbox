package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/lsariol/blackbox/internal/api"
)

// errReported fails a command whose problem has already been printed.
var errReported = errors.New("reported")

func (c *CLI) status(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "status"}
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	st, err := client.Status(ctx)
	if err != nil {
		return err
	}

	lag := "under 2 s"
	if st.LagSeconds >= 2 {
		lag = fmt.Sprintf("%d s", int(st.LagSeconds))
	}
	storage := fmt.Sprintf("%s of %s", size(uint64(st.StoredBytes)), size(uint64(st.MaxBytes)))
	if st.Oldest != nil {
		storage += ", oldest " + formatTime(*st.Oldest)
	}
	gaps := "none in the last 24 h"
	if st.Gaps24h > 0 {
		gaps = fmt.Sprintf("%d in the last 24 h: see \"gaps\"", st.Gaps24h)
	}
	fields([][2]string{
		{"Collector", collectorLine(st)},
		{"Streams", fmt.Sprintf("%d open, %d stopped %s read", st.StreamsOpen, st.StreamsRead, plural(st.StreamsRead, "container", "containers"))},
		{"Lag", lag},
		{"Storage", storage},
		{"Redacted", fmt.Sprintf("%d %s in the last 24 h", st.Redacted24h, plural64(st.Redacted24h, "line", "lines"))},
		{"Gaps", gaps},
	})
	if len(st.Attention) > 0 {
		warn("Needs attention: " + strings.Join(st.Attention, "; ") + ".")
		return errReported
	}
	return nil
}

// collectorLine says how the collector is doing.
func collectorLine(st api.StatusAnswer) string {
	if st.HeartbeatAt == nil {
		return "hasn't reported yet"
	}
	line := "version " + orDash(deref(st.Version))
	if st.LastSavedAt != nil {
		line += ", last saved " + formatTime(*st.LastSavedAt)
	}
	return line
}

func (c *CLI) sources(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "sources"}
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	answer, err := client.Sources(ctx)
	if err != nil {
		return err
	}
	rows := [][]string{{"PROJECT", "SERVICE", "VERSIONS", "NEWEST", "LINES", "STREAM"}}
	for _, s := range answer.Sources {
		stream := "closed"
		if s.OpenStreams > 0 {
			stream = "open"
		}
		rows = append(rows, []string{orDash(deref(s.Project)), s.Service, orDash(strings.Join(s.Versions, ", ")),
			formatTimePtr(s.Newest), strconv.FormatInt(s.Lines, 10), stream})
	}
	table(rows)
	info(fmt.Sprintf("%d %s", len(answer.Sources), plural(len(answer.Sources), "source", "sources")))
	return nil
}

func (c *CLI) gaps(ctx context.Context, args []string) error {
	const form = "gaps [--since 7d]"
	since := "7d"
	switch {
	case len(args) == 1:
	case len(args) == 3 && args[1] == "--since":
		since = args[2]
	default:
		return usageError{form: form}
	}
	span, err := parseSpan(since)
	if err != nil {
		return usageError{reason: fmt.Sprintf("--since %q isn't a span of time, such as 6h or 7d.", since), form: form}
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	answer, err := client.Gaps(ctx, c.opts.Now().Add(-span))
	if err != nil {
		return err
	}
	rows := [][]string{{"KIND", "CONTAINER", "FROM", "TO", "DETAIL"}}
	for _, g := range answer.Gaps {
		rows = append(rows, []string{g.Kind, orDash(deref(g.Container)), formatTime(g.From), formatTime(g.To), g.Detail})
	}
	table(rows)
	info(fmt.Sprintf("%d %s", len(answer.Gaps), plural(len(answer.Gaps), "gap", "gaps")))
	return nil
}

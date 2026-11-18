package cli

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const errorsForm = "errors [--since 24h] [--project NAME] [--level warn] [--limit N]"

// errors lists repeated warn and error lines, grouped by fingerprint.
func (c *CLI) errors(ctx context.Context, args []string) error {
	query := url.Values{}
	since := "24h"
	for i := 1; i < len(args); i++ {
		name, v, hasInline := strings.Cut(args[i], "=")
		switch name {
		case "--since", "--project", "--level", "--limit":
		default:
			return usageError{reason: fmt.Sprintf("Unknown argument %q.", args[i]), form: errorsForm}
		}
		if !hasInline {
			if i++; i >= len(args) {
				return usageError{reason: name + " needs a value.", form: errorsForm}
			}
			v = args[i]
		}
		if name == "--since" {
			since = v
			continue
		}
		query.Add(strings.TrimPrefix(name, "--"), v)
	}
	span, err := parseSpan(since)
	if err != nil {
		return usageError{reason: fmt.Sprintf("--since %q isn't a span of time, such as 6h or 7d.", since), form: errorsForm}
	}
	now := c.opts.Now()
	query.Set("from", now.Add(-span).UTC().Format(time.RFC3339))
	query.Set("to", now.UTC().Format(time.RFC3339))

	client, err := c.client()
	if err != nil {
		return err
	}
	answer, err := client.ErrorGroups(ctx, query)
	if err != nil {
		return err
	}
	rows := [][]string{{"COUNT", "LEVEL", "SOURCE", "FIRST", "LAST", "NEWEST", "FINGERPRINT"}}
	for _, g := range answer.Groups {
		source := g.Service
		if g.Project != nil {
			source = *g.Project + "/" + g.Service
		}
		rows = append(rows, []string{strconv.FormatInt(g.Count, 10), g.Level, source,
			formatTime(g.FirstSeen), formatTime(g.LastSeen), strconv.FormatInt(g.NewestID, 10), g.Fingerprint})
	}
	table(rows)
	n := len(answer.Groups)
	if int64(n) < answer.TotalGroups {
		info(fmt.Sprintf("%d of %d groups: add --limit for more", n, answer.TotalGroups))
	} else {
		info(fmt.Sprintf("%d %s", n, plural(n, "group", "groups")))
	}
	return nil
}

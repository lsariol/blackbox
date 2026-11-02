package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lsariol/blackbox/internal/api"
)

const searchForm = "search [text] [--since 1h] [--project NAME] [--service NAME] [--container NAME] [--version V] [--level LEVEL] [--stream NAME] [--field KEY=VALUE] [--oldest] [--limit N] [--cursor X] [--json]"

// lineTime is how a log line's time is shown: unlike other times in the CLI it
// has seconds, because lines from the same minute need telling apart.
const lineTime = "2006-01-02 15:04:05"

// parsed is a search's arguments.
type parsed struct {
	query  url.Values
	asJSON bool
}

// parseSearch reads search's arguments into API parameters.
func parseSearch(args []string, now time.Time) (parsed, error) {
	p := parsed{query: url.Values{}}
	var text []string
	since := ""
	value := func(i *int, name string) (string, error) {
		*i++
		if *i >= len(args) {
			return "", usageError{reason: name + " needs a value.", form: searchForm}
		}
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, inline, hasInline := strings.Cut(arg, "=")
		if !strings.HasPrefix(arg, "--") {
			text = append(text, arg)
			continue
		}
		next := func() (string, error) {
			if hasInline {
				return inline, nil
			}
			return value(&i, name)
		}
		var v string
		var err error
		switch name {
		case "--oldest":
			p.query.Set("order", "oldest")
		case "--json":
			p.asJSON = true
		case "--since":
			if since, err = next(); err != nil {
				return p, err
			}
		case "--from", "--to":
			if v, err = next(); err != nil {
				return p, err
			}
			t, err := parseTime(v)
			if err != nil {
				return p, usageError{reason: fmt.Sprintf("%s %q isn't a time.", name, v), form: searchForm}
			}
			p.query.Set(strings.TrimPrefix(name, "--"), t.UTC().Format(time.RFC3339Nano))
		case "--project", "--service", "--container", "--field":
			if v, err = next(); err != nil {
				return p, err
			}
			p.query.Add(strings.TrimPrefix(name, "--"), v)
		case "--version", "--level", "--stream", "--limit", "--cursor":
			if v, err = next(); err != nil {
				return p, err
			}
			p.query.Set(strings.TrimPrefix(name, "--"), v)
		default:
			return p, usageError{reason: fmt.Sprintf("Unknown flag %q.", name), form: searchForm}
		}
	}

	if since != "" && (p.query.Has("from") || p.query.Has("to")) {
		return p, usageError{reason: "Use --since or --from and --to, not both.", form: searchForm}
	}
	if !p.query.Has("from") && !p.query.Has("to") {
		if since == "" {
			since = "1h"
		}
		span, err := parseSpan(since)
		if err != nil {
			return p, usageError{reason: fmt.Sprintf("--since %q isn't a span of time, such as 30m, 6h or 7d.", since), form: searchForm}
		}
		p.query.Set("from", now.Add(-span).UTC().Format(time.RFC3339Nano))
		p.query.Set("to", now.UTC().Format(time.RFC3339Nano))
	}
	if len(text) > 0 {
		p.query.Set("q", strings.Join(text, " "))
	}
	return p, nil
}

// parseSpan reads 30m, 6h or 7d.
func parseSpan(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("too short")
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("not a positive number")
	}
	switch s[len(s)-1] {
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("no unit")
}

// parseTime reads a time in the server's zone, or RFC 3339.
func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	var last error
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		t, err := time.ParseInLocation(layout, s, time.Local)
		if err == nil {
			return t, nil
		}
		last = err
	}
	return time.Time{}, last
}

func (c *CLI) search(ctx context.Context, args []string) error {
	p, err := parseSearch(args[1:], c.opts.Now())
	if err != nil {
		return err
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	answer, err := client.Logs(ctx, p.query)
	if err != nil {
		return err
	}

	if p.asJSON {
		for _, r := range answer.Records {
			line, err := json.Marshal(r)
			if err != nil {
				return fmt.Errorf("JSON: %w", err)
			}
			out(string(line))
		}
	} else {
		rows := make([][]string, len(answer.Records))
		for i, r := range answer.Records {
			rows[i] = recordRow(r)
		}
		table(rows)
	}
	info(fmt.Sprintf("%d %s", len(answer.Records), plural(len(answer.Records), "line", "lines")))
	if n := answer.UnknownLeftOut; n > 0 {
		warn(fmt.Sprintf("%d %s in this range %s no level and %s left out. Search without --level to see them.",
			n, plural64(n, "line", "lines"), plural64(n, "has", "have"), plural64(n, "was", "were")))
	}
	if answer.NextCursor != nil {
		warn(fmt.Sprintf("More lines: add --cursor %s.", *answer.NextCursor))
	}
	return nil
}

// recordRow is one line in a list.
func recordRow(r api.Record) []string {
	return []string{
		strconv.FormatInt(r.ID, 10), r.Time.Local().Format(lineTime), r.Level, source(r),
		orDash(deref(r.Version)), lineText(r),
	}
}

// source is where a line came from: project/service, or the service alone.
func source(r api.Record) string {
	if r.Project == nil {
		return r.Service
	}
	return *r.Project + "/" + r.Service
}

// lineText is the message followed by its fields as key=value, sorted.
func lineText(r api.Record) string {
	text := r.Message
	for _, field := range sortedAttrs(r.Attrs) {
		text += " " + field[0] + "=" + field[1]
	}
	if r.Truncated {
		text += " (cut short)"
	}
	return text
}

// sortedAttrs reads a record's fields as text, sorted by name.
func sortedAttrs(raw json.RawMessage) [][2]string {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return [][2]string{{"attrs", string(raw)}}
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([][2]string, len(names))
	for i, name := range names {
		var text string
		if err := json.Unmarshal(fields[name], &text); err != nil {
			text = string(fields[name])
		}
		out[i] = [2]string{name, text}
	}
	return out
}

func (c *CLI) show(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return usageError{form: "show <id>"}
	}
	id, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return usageError{reason: fmt.Sprintf("%q isn't a line's number.", args[1]), form: "show <id>"}
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	r, err := client.Record(ctx, id)
	if err != nil {
		return err
	}
	rows := [][2]string{
		{"ID", strconv.FormatInt(r.ID, 10)},
		{"Time", r.Time.Local().Format("2006-01-02 15:04:05.000000 MST")},
		{"Level", r.Level},
		{"Project", orDash(deref(r.Project))},
		{"Service", r.Service},
		{"Container", r.Container},
		{"Image", r.Image},
		{"Version", orDash(deref(r.Version))},
		{"Stream", r.Stream},
		{"Message", r.Message},
	}
	if r.Truncated {
		rows = append(rows, [2]string{"Cut short", "yes: the line was longer than Blackbox keeps"})
	}
	if r.Redacted {
		rows = append(rows, [2]string{"Redacted", "yes: something in it looked like a secret"})
	}
	rows = append(rows, sortedAttrs(r.Attrs)...)
	fields(rows)
	return nil
}

// contextForm is how to call `context`.
const contextForm = "context <id> [--before N] [--after N]"

func (c *CLI) context(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return usageError{form: contextForm}
	}
	id, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return usageError{reason: fmt.Sprintf("%q isn't a line's number.", args[1]), form: contextForm}
	}
	before, after := 20, 20
	for i := 2; i < len(args); i += 2 {
		if i+1 >= len(args) || !slices.Contains([]string{"--before", "--after"}, args[i]) {
			return usageError{form: contextForm}
		}
		n, err := strconv.Atoi(args[i+1])
		if err != nil {
			return usageError{reason: fmt.Sprintf("%s %q isn't a number.", args[i], args[i+1]), form: contextForm}
		}
		if args[i] == "--before" {
			before = n
		} else {
			after = n
		}
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	answer, err := client.Context(ctx, id, before, after)
	if err != nil {
		return err
	}
	rows := make([][]string, len(answer.Records))
	for i, r := range answer.Records {
		mark := " "
		if r.ID == answer.Target {
			mark = ">"
		}
		rows[i] = []string{mark, strconv.FormatInt(r.ID, 10), r.Time.Local().Format(lineTime), r.Level, lineText(r)}
	}
	table(rows)
	info(fmt.Sprintf("%d %s", len(rows), plural(len(rows), "line", "lines")))
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func plural64(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

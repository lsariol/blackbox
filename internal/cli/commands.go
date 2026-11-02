package cli

import (
	"context"
	"fmt"
	"strings"
)

// command is one CLI command; run gets the full arguments, args[0] included.
type command struct {
	names []string

	group    string
	synopsis string
	summary  string

	usages   []usage
	flags    []flag
	examples []example

	run func(c *CLI, ctx context.Context, args []string) error

	complete func(c *CLI) []string
}

// usage is one way to call a command, e.g. "show <id>", and what it does.
type usage struct {
	form string
	help string
}

// flag is an option a command takes, e.g. "--since 1h".
type flag struct {
	name string
	help string
}

// example is a command line and a short note on what it does.
type example struct {
	line string
	note string
}

// usageError is returned when a command is called with the wrong arguments.
type usageError struct {
	reason string
	form   string
}

func (e usageError) Error() string {
	if e.reason == "" {
		return "Usage: " + e.form
	}
	return e.reason + " Usage: " + e.form
}

// Command groups, in the order `help` shows them; empty groups are left out.
const (
	groupSearch   = "Search"
	groupOverview = "Overview"
	groupSetup    = "Setup"
)

var groupOrder = []string{groupSearch, groupOverview, groupSetup}

// commandTable lists every command.
func commandTable() []command {
	return []command{
		{
			names:   []string{"version"},
			group:   groupSetup,
			summary: "Blackbox's version",
			usages: []usage{
				{"version", "Print the version this Blackbox runs: the release (v0.0.0), or dev-<commit> for a local build."},
			},
			run: (*CLI).version,
		},
		{
			names:    []string{"help", "h"},
			group:    groupSetup,
			synopsis: "[command]",
			summary:  "This overview, or one command in detail",
			usages: []usage{
				{"help", "List every command."},
				{"help <command>", "Show one command in detail, with examples."},
			},
			examples: []example{{"help version", "everything about version"}},
			run:      (*CLI).help,
			complete: (*CLI).commandNames,
		},
		{
			names:   []string{"exit", "quit"},
			group:   groupSetup,
			summary: "Leave the shell (Blackbox keeps running)",
			usages:  []usage{{"exit", "Leave the shell. Blackbox keeps running."}},
			run:     (*CLI).exit,
		},
	}
}

func (c *CLI) version(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "version"}
	}
	out(c.opts.Version)
	return nil
}

// help shows the overview or one command in detail.
func (c *CLI) help(ctx context.Context, args []string) error {
	switch len(args) {
	case 1:
		out(c.overview())
		return nil
	case 2:
		cmd, ok := c.byName[strings.ToLower(args[1])]
		if !ok {
			return usageError{reason: fmt.Sprintf("Unknown command %q.", args[1]), form: "help [command]"}
		}
		out(commandHelp(*cmd))
		return nil
	default:
		return usageError{form: "help [command]"}
	}
}

// overview lists every command on one line, by group.
func (c *CLI) overview() string {
	type line struct{ left, right string }
	byGroup := make(map[string][]line)
	width := 0
	for _, cmd := range c.commands {
		left := strings.TrimSpace(cmd.names[0] + " " + cmd.synopsis)
		byGroup[cmd.group] = append(byGroup[cmd.group], line{left, cmd.summary})
		width = max(width, len(left))
	}

	var b strings.Builder
	for _, group := range groupOrder {
		lines := byGroup[group]
		if len(lines) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(group + ":\n")
		for _, l := range lines {
			fmt.Fprintf(&b, "  %-*s  %s\n", width, l.left, l.right)
		}
	}
	b.WriteString("\nType \"help <command>\" for details and examples, e.g. \"help search\".")
	return b.String()
}

// helpWidth is the widest line help text wraps to.
const helpWidth = 80

// commandHelp is `help <command>`: a header, then usage, flags and examples.
func commandHelp(cmd command) string {
	var b strings.Builder

	header := cmd.names[0]
	if len(cmd.names) > 1 {
		header += " (" + strings.Join(cmd.names[1:], ", ") + ")"
	}
	b.WriteString(header + ": " + cmd.summary + "\n")

	var usages, flags, examples [][2]string
	for _, u := range cmd.usages {
		usages = append(usages, [2]string{u.form, u.help})
	}
	for _, f := range cmd.flags {
		flags = append(flags, [2]string{f.name, f.help})
	}
	for _, e := range cmd.examples {
		examples = append(examples, [2]string{e.line, e.note})
	}

	width := columnWidth(append(append([][2]string{}, usages...), flags...), 30)
	b.WriteString("\nUsage:\n" + columns(usages, width))
	if len(flags) > 0 {
		b.WriteString("\nFlags:\n" + columns(flags, width))
	}
	if len(examples) > 0 {
		b.WriteString("\nExamples:\n" + columns(examples, columnWidth(examples, 46)))
	}
	return strings.TrimRight(b.String(), "\n")
}

// columnWidth is the width of the left column for rows: the longest left
// side, not counting those longer than maxLeft (they get their own line).
func columnWidth(rows [][2]string, maxLeft int) int {
	width := 0
	for _, r := range rows {
		if len(r[0]) <= maxLeft {
			width = max(width, len(r[0]))
		}
	}
	return width
}

// columns lays out rows as two columns: the left one indented by two spaces
// and width wide, the right one wrapped to helpWidth.
func columns(rows [][2]string, width int) string {
	col := 2 + width + 3
	pad := strings.Repeat(" ", col)

	var b strings.Builder
	for _, r := range rows {
		lines := wrap(r[1], helpWidth-col)
		if len(r[0]) > width {
			b.WriteString("  " + r[0] + "\n")
			for _, l := range lines {
				b.WriteString(pad + l + "\n")
			}
			continue
		}
		for i, l := range lines {
			left := ""
			if i == 0 {
				left = r[0]
			}
			fmt.Fprintf(&b, "  %-*s   %s\n", width, left, l)
		}
	}
	return b.String()
}

// wrap splits text into lines of at most width characters, at spaces.
func wrap(text string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	return append(lines, line)
}

func (c *CLI) exit(ctx context.Context, args []string) error {
	c.quit = true
	return nil
}

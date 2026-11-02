package cli

import (
	"context"
	"io"
	"os"
	"sort"
	"strings"

	"golang.org/x/term"
)

// runTerminal runs the prompt with line editing: arrow keys, history
// (up/down) and Tab completion. It returns false if the terminal can't be
// put in raw mode, so Run falls back to plain lines.
func (c *CLI) runTerminal(ctx context.Context) bool {
	fd := int(os.Stdin.Fd())

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return false
	}
	defer term.Restore(fd, oldState)

	c.term = term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{os.Stdin, os.Stderr}, c.prompt)
	c.term.AutoCompleteCallback = c.complete

	oldOut, oldErr := stdout, stderr
	stdout, stderr = c.term, c.term
	defer func() { stdout, stderr = oldOut, oldErr }()

	for ctx.Err() == nil && !c.quit {
		line, err := c.term.ReadLine()
		if err != nil {
			return true
		}
		c.runLine(ctx, line)
	}
	return true
}

// complete handles Tab: command names first, then the command's own
// completions.
func (c *CLI) complete(line string, pos int, key rune) (string, int, bool) {
	if key != '\t' {
		return "", 0, false
	}

	head, tail := line[:pos], line[pos:]
	start := strings.LastIndex(head, " ") + 1
	word := head[start:]
	before := strings.Fields(head[:start])

	var candidates []string
	switch len(before) {
	case 0:
		candidates = c.commandNames()
	case 1:
		cmd, ok := c.byName[strings.ToLower(before[0])]
		if !ok || cmd.complete == nil {
			return "", 0, false
		}
		candidates = cmd.complete(c)
	default:
		return "", 0, false
	}

	var matches []string
	for _, candidate := range candidates {
		if strings.HasPrefix(strings.ToLower(candidate), strings.ToLower(word)) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return "", 0, false
	}

	completed := commonPrefix(matches)
	if len(matches) == 1 {
		completed += " "
	} else if len(completed) <= len(word) {
		if c.term != nil {
			c.term.Write([]byte(strings.Join(matches, "  ") + "\n"))
		}
		return "", 0, false
	}
	return head[:start] + completed + tail, start + len(completed), true
}

// commandNames returns every command's primary name, sorted.
func (c *CLI) commandNames() []string {
	names := make([]string, 0, len(c.commands))
	for _, cmd := range c.commands {
		names = append(names, cmd.names[0])
	}
	sort.Strings(names)
	return names
}

func commonPrefix(words []string) string {
	prefix := words[0]
	for _, w := range words[1:] {
		for !strings.HasPrefix(strings.ToLower(w), strings.ToLower(prefix)) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	return prefix
}

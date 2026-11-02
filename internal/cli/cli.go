// Package cli is Blackbox's command line: the shell and one-shot commands. It
// is a thin client of the API, so it and Vista always agree. Every command is
// read-only.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// Options are the CLI's settings.
type Options struct {
	Env     string
	Version string
	Now     func() time.Time // time.Now unless set
}

// CLI runs Blackbox's commands, one at a time or as a shell.
type CLI struct {
	opts   Options
	prompt string

	scanner     *bufio.Scanner
	term        *term.Terminal
	interactive bool

	commands []command
	byName   map[string]*command

	quit bool
}

// New returns a CLI with every command.
func New(opts Options) *CLI {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	c := &CLI{
		opts:        opts,
		prompt:      promptFor(opts.Env),
		scanner:     bufio.NewScanner(os.Stdin),
		interactive: term.IsTerminal(int(os.Stdin.Fd())),
		commands:    commandTable(),
		byName:      make(map[string]*command),
	}
	for i := range c.commands {
		for _, name := range c.commands[i].names {
			c.byName[name] = &c.commands[i]
		}
	}
	return c
}

// Run reads and runs commands until stdin closes, `exit` is typed, or ctx is
// cancelled.
func (c *CLI) Run(ctx context.Context) {
	if c.interactive && c.runTerminal(ctx) {
		return
	}
	for ctx.Err() == nil && !c.quit {
		fmt.Fprint(stderr, c.prompt)
		if !c.scanner.Scan() {
			return
		}
		c.runLine(ctx, c.scanner.Text())
	}
}

// runLine runs one typed line, reporting what goes wrong.
func (c *CLI) runLine(ctx context.Context, line string) {
	args, err := splitArgs(line)
	if err != nil {
		warn(err.Error())
		return
	}
	if len(args) > 0 {
		report(c.Exec(ctx, args))
	}
}

// Exec runs a single command, e.g. ["help", "version"].
func (c *CLI) Exec(ctx context.Context, args []string) error {
	cmd, ok := c.byName[strings.ToLower(args[0])]
	if !ok {
		return fmt.Errorf("Unknown command %q. Type \"help\" to see the available commands.", args[0])
	}
	return cmd.run(c, ctx, args)
}

// Report shows an error returned by Exec: wrong arguments as a warning,
// anything else as an error.
func Report(err error) {
	report(err)
}

func report(err error) {
	if err == nil {
		return
	}
	var usage usageError
	if errors.As(err, &usage) {
		warn(err.Error())
		return
	}
	fail(err.Error())
}

// splitArgs splits a typed line at spaces, keeping "quoted text" together.
func splitArgs(line string) ([]string, error) {
	var args []string
	var word strings.Builder
	var quote rune
	inWord := false
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				args, inWord = append(args, word.String()), false
				word.Reset()
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("Unclosed quote: add a closing %c.", quote)
	}
	if inWord {
		args = append(args, word.String())
	}
	return args, nil
}

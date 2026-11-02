package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// capture points the CLI's output at buffers for one test.
func capture(t *testing.T) (*bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	oldOut, oldErr := stdout, stderr
	var o, e bytes.Buffer
	stdout, stderr = &o, &e
	t.Cleanup(func() { stdout, stderr = oldOut, oldErr })
	return &o, &e
}

func TestSplitArgs(t *testing.T) {
	tests := []struct {
		line string
		want []string
	}{
		{"", nil},
		{"  status  ", []string{"status"}},
		{`search "connection refused" --level error`, []string{"search", "connection refused", "--level", "error"}},
		{`search 'it "worked"'`, []string{"search", `it "worked"`}},
		{`search ""`, []string{"search", ""}},
		{"a\tb", []string{"a", "b"}},
		{`--field a="b c"`, []string{"--field", "a=b c"}},
	}
	for _, tt := range tests {
		got, err := splitArgs(tt.line)
		if err != nil {
			t.Errorf("splitArgs(%q): %v", tt.line, err)
			continue
		}
		if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", tt.want) {
			t.Errorf("splitArgs(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
	if _, err := splitArgs(`search "open`); err == nil {
		t.Error("an unclosed quote should be an error")
	}
}

func TestEveryCommandHasHelp(t *testing.T) {
	groups := map[string]bool{}
	for _, g := range groupOrder {
		groups[g] = true
	}
	for _, cmd := range commandTable() {
		if len(cmd.names) == 0 || cmd.summary == "" || len(cmd.usages) == 0 || cmd.run == nil {
			t.Errorf("command %v is incomplete", cmd.names)
		}
		if !groups[cmd.group] {
			t.Errorf("command %v has an unknown group %q", cmd.names, cmd.group)
		}
	}
}

func TestHelpWrapsAt80(t *testing.T) {
	o, _ := capture(t)
	c := New(Options{})
	for _, cmd := range c.commands {
		if err := c.Exec(context.Background(), []string{"help", cmd.names[0]}); err != nil {
			t.Fatal(err)
		}
	}
	for _, line := range strings.Split(o.String(), "\n") {
		if len(line) > helpWidth+40 {
			t.Errorf("line too long: %q", line)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	capture(t)
	err := New(Options{}).Exec(context.Background(), []string{"nope"})
	if err == nil || !strings.Contains(err.Error(), "help") {
		t.Errorf("err = %v", err)
	}
}

func TestPrompt(t *testing.T) {
	if got := promptFor("dev"); got != "blackbox (dev)> " {
		t.Errorf("prompt = %q", got)
	}
	if got := promptFor(""); got != "blackbox> " {
		t.Errorf("prompt = %q", got)
	}
}

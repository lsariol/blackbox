package cli

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"golang.org/x/term"
)

// Data goes to stdout; messages go to stderr with a symbol (✓ ! ✗ ?), colored only on a terminal without NO_COLOR.
var (
	stdout   io.Writer = os.Stdout
	stderr   io.Writer = os.Stderr
	useColor           = colorEnabled()
)

const (
	yellow = "\033[33m"
	red    = "\033[31m"
	reset  = "\033[0m"
)

func colorEnabled() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	return term.IsTerminal(int(os.Stderr.Fd()))
}

func colorize(color string, s string) string {
	if !useColor {
		return s
	}
	return color + s + reset
}

// clean replaces control characters, except line breaks, with "?". Much of
// what the CLI prints comes from outside (container names, labels, websites'
// errors), and a control character could take over the terminal.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

// cell is clean for one table cell or field: on a single line.
func cell(s string) string {
	return strings.ReplaceAll(clean(s), "\n", " ")
}

// out writes data to stdout, followed by a newline.
func out(s string) {
	fmt.Fprintln(stdout, clean(s))
}

// warn reports something the user should notice or fix, e.g. wrong arguments.
func warn(msg string) {
	fmt.Fprintln(stderr, colorize(yellow, "! "+clean(msg)))
}

// fail reports an error.
func fail(msg string) {
	fmt.Fprintln(stderr, colorize(red, "✗ "+clean(msg)))
}

// info reports something neutral, such as a count after a list.
func info(msg string) {
	fmt.Fprintln(stderr, clean(msg))
}

// promptFor returns the prompt for an environment: "blackbox (dev)> ", or
// "blackbox (prod)> " with the label in red.
func promptFor(env string) string {
	env = strings.ToLower(strings.TrimSpace(env))
	if env == "" {
		return "blackbox> "
	}
	label := "(" + env + ")"
	if env == "prod" {
		label = colorize(red, label)
	}
	return "blackbox " + label + "> "
}

// table writes rows as aligned columns to stdout.
func table(rows [][]string) {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = cell(c)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	tw.Flush()
}

// fields writes "Name:  value" lines with the values aligned, to stdout.
func fields(rows [][2]string) {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintf(tw, "%s:\t%s\n", cell(r[0]), cell(r[1]))
	}
	tw.Flush()
}

// formatTime is a moment in local time, "2006-01-02 15:04", or "-".
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return formatTime(*t)
}

// age is how long ago t was, roughly: "4 s", "3 min", "5 h", "2 days".
func age(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

// duration is a long span: "79 days, 8 h", "5 h, 12 min", "40 s".
func duration(d time.Duration) string {
	days, hours, mins := int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60
	switch {
	case days > 0:
		return fmt.Sprintf("%d %s, %d h", days, plural(days, "day", "days"), hours)
	case hours > 0:
		return fmt.Sprintf("%d h, %d min", hours, mins)
	case mins > 0:
		return fmt.Sprintf("%d min", mins)
	}
	return fmt.Sprintf("%d s", int(d.Seconds()))
}

// size is a byte count in binary units: "512 KiB", "3.2 GiB".
func size(n uint64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n) / 1024
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func sizePtr(n *uint64) string {
	if n == nil {
		return "-"
	}
	return size(*n)
}

// rateOf is bytes per second: "12.0 KiB/s", or "-".
func rateOf(v *float64) string {
	if v == nil {
		return "-"
	}
	return size(uint64(math.Round(*v))) + "/s"
}

func percent(v float64) string {
	return fmt.Sprintf("%.1f%%", v)
}

func percentPtr(v *float64) string {
	if v == nil {
		return "-"
	}
	return percent(*v)
}

// cores is CPU use in cores: "0.50 cores", or "-".
func cores(v *float64) string {
	if v == nil {
		return "-"
	}
	if *v == 1 {
		return "1.00 core"
	}
	return fmt.Sprintf("%.2f cores", *v)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func plural(n int, one string, many string) string {
	if n == 1 {
		return one
	}
	return many
}

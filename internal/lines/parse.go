package lines

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Levels, as stored.
const (
	LevelUnknown int16 = 0
	LevelDebug   int16 = 10
	LevelInfo    int16 = 20
	LevelWarn    int16 = 30
	LevelError   int16 = 40
)

// Formats, as stored.
const (
	FormatPlain       int16 = 0
	FormatJSON        int16 = 1
	FormatSlogText    int16 = 2
	FormatGoLog       int16 = 3
	FormatCloudflared int16 = 4
	FormatPostgres    int16 = 5
)

// Record is a line made ready to store.
type Record struct {
	Level     int16
	Format    int16
	Message   string
	Attrs     []byte // JSON, nil without fields
	Truncated bool
	Redacted  bool
}

// LevelName is a level's name for the API and CLI.
func LevelName(level int16) string {
	switch level {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	}
	return "unknown"
}

// LevelFromName is the level with that name; ok is false for any other.
func LevelFromName(name string) (int16, bool) {
	for _, l := range []int16{LevelDebug, LevelInfo, LevelWarn, LevelError} {
		if LevelName(l) == name {
			return l, true
		}
	}
	return 0, false
}

// StreamName is a stream's name for the API and CLI.
func StreamName(stream int16) string {
	if stream == Stderr {
		return "stderr"
	}
	return "stdout"
}

var (
	goLogPrefix = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(\.\d+)? `)
	tagPrefix   = regexp.MustCompile(`^\[([A-Za-z0-9 _-]{1,32})\] `)
	cloudflared = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z (DBG|INF|WRN|ERR|FTL) (.*)$`)
	postgres    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(\.\d+)? \S+ \[\d+\] (DEBUG\d?|LOG|INFO|NOTICE|WARNING|ERROR|FATAL|PANIC):  (.*)$`)
)

// Parse makes a line safe to store, reads what it can from it, and redacts it.
func Parse(text []byte) Record {
	line, truncated := safe(text)
	var rec Record
	var attrs map[string]any
	switch {
	case strings.HasPrefix(line, "{"):
		rec, attrs = parseJSON(line)
	case strings.HasPrefix(line, "time=") || strings.HasPrefix(line, "level="):
		rec, attrs = parseSlogText(line)
	case goLogPrefix.MatchString(line):
		rec, attrs = parseGoLog(line)
	case cloudflared.MatchString(line):
		m := cloudflared.FindStringSubmatch(line)
		rec = Record{Level: cloudflaredLevel(m[1]), Format: FormatCloudflared, Message: m[2]}
	case postgres.MatchString(line):
		m := postgres.FindStringSubmatch(line)
		rec = Record{Level: postgresLevel(m[2]), Format: FormatPostgres, Message: m[3]}
	case strings.HasPrefix(line, "panic: ") || strings.HasPrefix(line, "fatal error: "):
		rec = Record{Level: LevelError, Message: line}
	default:
		rec = Record{Level: LevelUnknown, Message: line}
	}
	rec.Truncated = truncated

	message, count := redactText(rec.Message)
	rec.Message = message
	if attrs != nil {
		count += redactMap(attrs)
		if encoded, err := json.Marshal(attrs); err == nil && len(attrs) > 0 {
			rec.Attrs = encoded
		}
	}
	rec.Redacted = count > 0
	return rec
}

// safe makes text storable: valid UTF-8, no NUL bytes, at most maxLine bytes.
func safe(text []byte) (string, bool) {
	truncated := len(text) > maxLine
	if truncated {
		text = text[:maxLine]
		for len(text) > 0 && !utf8.RuneStart(text[len(text)-1]) {
			text = text[:len(text)-1]
		}
		if len(text) > 0 && !utf8.Valid(text[len(text)-1:]) {
			text = text[:len(text)-1]
		}
	}
	return cleanString(string(text)), truncated
}

// cleanString keeps a string storable in text and jsonb columns.
func cleanString(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "")
}

// parseJSON reads a JSON object with a level and a message; anything else is
// plain text.
func parseJSON(line string) (Record, map[string]any) {
	plain := Record{Level: LevelUnknown, Message: line}
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil || dec.More() {
		return plain, nil
	}
	levelKey, ok := firstString(fields, "level", "severity")
	if !ok {
		return plain, nil
	}
	msgKey, ok := firstString(fields, "msg", "message")
	if !ok {
		return plain, nil
	}
	level, ok := levelOf(fields[levelKey].(string))
	if !ok {
		return plain, nil
	}
	rec := Record{Level: level, Format: FormatJSON, Message: cleanString(fields[msgKey].(string))}
	delete(fields, levelKey)
	delete(fields, msgKey)
	delete(fields, "time")
	return rec, cleanMap(fields)
}

func firstString(fields map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if _, ok := fields[k].(string); ok {
			return k, true
		}
	}
	return "", false
}

// cleanMap makes decoded JSON safe for a jsonb column.
func cleanMap(fields map[string]any) map[string]any {
	out := make(map[string]any, len(fields))
	for k, v := range fields {
		out[cleanString(k)] = cleanValue(v)
	}
	return out
}

func cleanValue(v any) any {
	switch v := v.(type) {
	case string:
		return cleanString(v)
	case json.Number:
		if f, err := v.Float64(); err != nil || math.IsInf(f, 0) {
			return v.String()
		}
		return v
	case []any:
		for i := range v {
			v[i] = cleanValue(v[i])
		}
		return v
	case map[string]any:
		return cleanMap(v)
	}
	return v
}

// parseSlogText reads slog's text format: key=value pairs, with values quoted
// when they need it.
func parseSlogText(line string) (Record, map[string]any) {
	plain := Record{Level: LevelUnknown, Message: line}
	pairs, ok := splitPairs(line)
	if !ok {
		return plain, nil
	}
	levelText, hasLevel := pairs["level"]
	message, hasMessage := pairs["msg"]
	if !hasLevel || !hasMessage {
		return plain, nil
	}
	level, ok := levelOf(levelText)
	if !ok {
		return plain, nil
	}
	attrs := make(map[string]any, len(pairs))
	for k, v := range pairs {
		if k != "time" && k != "level" && k != "msg" {
			attrs[k] = v
		}
	}
	return Record{Level: level, Format: FormatSlogText, Message: message}, attrs
}

// splitPairs splits key=value text; ok is false if it isn't all pairs.
func splitPairs(line string) (map[string]string, bool) {
	pairs := make(map[string]string)
	for len(line) > 0 {
		key, rest, found := strings.Cut(line, "=")
		if !found || key == "" || strings.ContainsAny(key, " \"") {
			return nil, false
		}
		var value string
		if strings.HasPrefix(rest, `"`) {
			quoted, err := strconv.QuotedPrefix(rest)
			if err != nil {
				return nil, false
			}
			value, err = strconv.Unquote(quoted)
			if err != nil {
				return nil, false
			}
			rest = rest[len(quoted):]
			if rest != "" && rest[0] != ' ' {
				return nil, false
			}
			rest = strings.TrimPrefix(rest, " ")
		} else {
			value, rest, _ = strings.Cut(rest, " ")
		}
		pairs[key] = cleanString(value)
		line = rest
	}
	return pairs, true
}

// parseGoLog reads a line from Go's log package: its timestamp is dropped,
// since Docker's is kept. The CLI symbols are the only level it has.
func parseGoLog(line string) (Record, map[string]any) {
	rest := goLogPrefix.ReplaceAllString(line, "")
	level := LevelUnknown
	switch {
	case strings.HasPrefix(rest, "✗ "):
		level, rest = LevelError, strings.TrimPrefix(rest, "✗ ")
	case strings.HasPrefix(rest, "! "):
		level, rest = LevelWarn, strings.TrimPrefix(rest, "! ")
	case strings.HasPrefix(rest, "✓ "):
		level, rest = LevelInfo, strings.TrimPrefix(rest, "✓ ")
	}
	var attrs map[string]any
	if m := tagPrefix.FindStringSubmatch(rest); m != nil {
		attrs = map[string]any{"component": m[1]}
		rest = rest[len(m[0]):]
	}
	return Record{Level: level, Format: FormatGoLog, Message: rest}, attrs
}

// levelOf reads a level name, with slog's offset if it has one (ERROR+2).
func levelOf(text string) (int16, bool) {
	name, offsetText := text, ""
	if i := strings.IndexAny(text, "+-"); i > 0 {
		name, offsetText = text[:i], text[i:]
	}
	var base int
	switch strings.ToLower(name) {
	case "trace", "debug":
		base = -4
	case "info", "notice":
		base = 0
	case "warn", "warning":
		base = 4
	case "error", "err", "critical", "fatal", "panic":
		base = 8
	default:
		return 0, false
	}
	if offsetText != "" {
		offset, err := strconv.Atoi(offsetText)
		if err != nil {
			return 0, false
		}
		base = min(max(base+min(max(offset, -100), 100), -100), 100)
	}
	switch {
	case base < 0:
		return LevelDebug, true
	case base < 4:
		return LevelInfo, true
	case base < 8:
		return LevelWarn, true
	}
	return LevelError, true
}

func cloudflaredLevel(word string) int16 {
	switch word {
	case "DBG":
		return LevelDebug
	case "INF":
		return LevelInfo
	case "WRN":
		return LevelWarn
	}
	return LevelError
}

func postgresLevel(word string) int16 {
	switch word {
	case "LOG", "INFO", "NOTICE":
		return LevelInfo
	case "WARNING":
		return LevelWarn
	case "ERROR", "FATAL", "PANIC":
		return LevelError
	}
	return LevelDebug
}

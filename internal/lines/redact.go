package lines

import (
	"regexp"
	"strings"
)

// redacted replaces every hidden value, as blackboxlog does.
const redacted = "[redacted]"

// sensitiveWords mark a field name as sensitive, as they do in blackboxlog.
var sensitiveWords = []string{
	"password", "passwd", "secret", "token", "api_key", "apikey", "authorization", "cookie",
}

// patterns find secrets in text. They're a safety net and can miss things:
// the real protection is not logging secrets. Each keeps its first group and
// replaces the rest.
var patterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(\bbearer\s+)[a-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://[^\s:/@]*:)[^\s/@]+(?:@)`),
	regexp.MustCompile(`(?i)(\b(?:set-)?(?:authorization|cookie)["']?\s*[:=]\s*).*$`),
	regexp.MustCompile(`(?i)([a-z0-9_-]*(?:password|passwd|secret|token|api[_-]?key)(?:[_-][a-z0-9_-]+)?["']?\s*[:=]\s*["']?)[^\s"'&,;]+`),
}

// urlPassword is the pattern that needs its closing @ put back.
const urlPassword = 1

// redactText hides what the patterns find, and says how many it hid.
func redactText(s string) (string, int) {
	count := 0
	for i, p := range patterns {
		s = p.ReplaceAllStringFunc(s, func(match string) string {
			group := p.FindStringSubmatch(match)
			if strings.HasPrefix(match[len(group[1]):], redacted) {
				return match
			}
			count++
			if i == urlPassword {
				return group[1] + redacted + "@"
			}
			return group[1] + redacted
		})
	}
	return s, count
}

// redactMap hides sensitive fields and scans the rest, in place.
func redactMap(fields map[string]any) int {
	count := 0
	for k, v := range fields {
		if sensitiveKey(k) {
			if v != redacted {
				fields[k] = redacted
				count++
			}
			continue
		}
		var n int
		fields[k], n = redactValue(v)
		count += n
	}
	return count
}

func redactValue(v any) (any, int) {
	switch v := v.(type) {
	case string:
		return redactText(v)
	case []any:
		count := 0
		for i := range v {
			var n int
			v[i], n = redactValue(v[i])
			count += n
		}
		return v, count
	case map[string]any:
		return v, redactMap(v)
	}
	return v, 0
}

func sensitiveKey(key string) bool {
	key = strings.ReplaceAll(strings.ToLower(key), "-", "_")
	for _, word := range sensitiveWords {
		if strings.Contains(key, word) {
			return true
		}
	}
	return false
}

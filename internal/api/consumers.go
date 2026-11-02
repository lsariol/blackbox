package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"strings"
)

// scope is what a consumer may read.
type scope string

// logsRead is the one scope: reading logs and collection status.
const logsRead scope = "logs:read"

// consumer is a caller allowed to use the API. Its token is the Cove key Key,
// passed through to the container under the same name.
type consumer struct {
	Name   string
	Key    string
	Scopes []scope
}

// consumers are the callers allowed to use the API. Adding one: a line here,
// its key in Cove, and its pass-through line in docker-compose.yml.
var consumers = []consumer{
	{Name: "cli", Key: "BLACKBOX_CLI_TOKEN", Scopes: []scope{logsRead}},
}

// minTokenLength is the shortest token the API accepts from Cove.
const minTokenLength = 32

// credential is a consumer and its token's digest; the token itself isn't
// kept.
type credential struct {
	consumer consumer
	digest   [sha256.Size]byte
}

// loadCredentials reads each consumer's token. It refuses a missing or short
// one, so a deploy without the key fails instead of serving without it.
func loadCredentials(getenv func(string) string, list []consumer) ([]credential, error) {
	creds := make([]credential, 0, len(list))
	for _, c := range list {
		token := strings.TrimSpace(getenv(c.Key))
		switch {
		case token == "":
			return nil, fmt.Errorf("api: %s's token, %s, isn't set: create it in Cove (generate %s 64) and pass it through in docker-compose.yml", c.Name, c.Key, c.Key)
		case len(token) < minTokenLength:
			return nil, fmt.Errorf("api: %s's token, %s, is %d characters; it needs at least %d: generate a new one in Cove (generate %s 64)", c.Name, c.Key, len(token), minTokenLength, c.Key)
		}
		creds = append(creds, credential{consumer: c, digest: sha256.Sum256([]byte(token))})
	}
	return creds, nil
}

// identify returns the consumer whose token this is. Every digest is
// compared in constant time, so timing says nothing about near misses.
func identify(creds []credential, token string) (consumer, bool) {
	digest := sha256.Sum256([]byte(token))
	var found consumer
	ok := false
	for _, c := range creds {
		if subtle.ConstantTimeCompare(digest[:], c.digest[:]) == 1 {
			found, ok = c.consumer, true
		}
	}
	return found, ok
}

func (c consumer) may(s scope) bool {
	for _, have := range c.Scopes {
		if have == s {
			return true
		}
	}
	return false
}

// ConsumerInfo is a consumer as `blackbox consumers` shows it. It never holds the
// token, only whether a usable one is set.
type ConsumerInfo struct {
	Name   string
	Key    string
	Scopes []string
	Token  string // "present", "missing" or "too short"
}

// Consumers describes every consumer and its token's state.
func Consumers(getenv func(string) string) []ConsumerInfo {
	out := make([]ConsumerInfo, 0, len(consumers))
	for _, c := range consumers {
		info := ConsumerInfo{Name: c.Name, Key: c.Key, Token: "present"}
		for _, s := range c.Scopes {
			info.Scopes = append(info.Scopes, string(s))
		}
		switch token := strings.TrimSpace(getenv(c.Key)); {
		case token == "":
			info.Token = "missing"
		case len(token) < minTokenLength:
			info.Token = "too short"
		}
		out = append(out, info)
	}
	return out
}

// Package containers is all that leaves blackbox-proxy, besides log bytes: the container list and streams.
package containers

import "time"

// Socket is where blackbox-proxy listens, in the proxy volume.
const Socket = "/run/blackbox-proxy/proxy.sock"

// The labels the proxy reads; every other label stays behind.
const (
	ProjectLabel = "com.docker.compose.project"
	ServiceLabel = "com.docker.compose.service"
	VersionLabel = "com.mobasity.version"
	LogsLabel    = "com.mobasity.logs" // "off" leaves a container out
)

// LogsOff is the LogsLabel value that leaves a container out of collection.
const LogsOff = "off"

// Container is one container: what it is and whether its logs can be read.
type Container struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Project    string     `json:"project"` // "" without compose labels
	Service    string     `json:"service"` // the container name without compose labels
	Image      string     `json:"image"`   // the reference it runs, e.g. postgres:16.4
	State      string     `json:"state"`   // created, running, paused, restarting, removing, exited, dead
	TTY        bool       `json:"tty"`     // its stream has one combined output, without framing
	LogDriver  string     `json:"log_driver"`
	Version    string     `json:"version"`
	Excluded   bool       `json:"excluded"` // com.mobasity.logs=off
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

// Active reports whether the container can still write logs.
func (c Container) Active() bool {
	switch c.State {
	case "running", "paused", "restarting":
		return true
	}
	return false
}

// ContainersAnswer is GET /v1/containers: every container, and apart those Docker couldn't inspect.
type ContainersAnswer struct {
	Containers []Container      `json:"containers"`
	Failed     []ContainerError `json:"failed"`
}

// ContainerError is a container Docker couldn't inspect, and why.
type ContainerError struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// LatestAnswer is GET /v1/logs/{id}/latest: the newest line's time, or nil; never its text.
type LatestAnswer struct {
	Latest *time.Time `json:"latest"`
}

// Start says where a log stream begins.
type Start struct {
	SinceNanos int64 // Unix nanoseconds; lines at this time or later
	FromEnd    bool  // new lines only, for a log damaged before its end
}

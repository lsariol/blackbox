// Package collector is blackbox-collector: it follows every container's log
// through the proxy and saves the lines. One loop owns all the state; each
// stream only reads and parses, then hands its lines to the loop.
//
// There is no retry queue. Docker's log files are the buffer: a line counts as
// read once its transaction commits, and after any failure the streams reopen
// from the saved checkpoints.
package collector

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/lsariol/blackbox/internal/containers"
	"github.com/lsariol/blackbox/internal/lines"
	"github.com/lsariol/blackbox/internal/store"
)

// HeartbeatFile is what the collector's healthcheck reads, in its tmpfs.
const HeartbeatFile = "/run/blackbox/heartbeat"

// heartbeatLimit is how old the heartbeat may be for the collector to be healthy.
const heartbeatLimit = 30 * time.Second

const (
	maxBatchLines = 1000
	maxBatchBytes = 1 << 20

	// callTimeout bounds one database or proxy call from the loop.
	callTimeout = 5 * time.Second

	// retainTimeout bounds one retention run.
	retainTimeout = 60 * time.Second

	// downAfter is how long the collector must have been gone to note a gap.
	downAfter = 60 * time.Second

	// minGap is the shortest stretch worth recording for a damaged log.
	minGap = time.Second

	// orderSlack allows for stdout and stderr lines stamped slightly out of order.
	orderSlack = int64(time.Millisecond)
)

// timing holds the loop's intervals; tests shorten them.
type timing struct {
	flush       time.Duration
	scan        time.Duration
	report      time.Duration
	stuck       time.Duration
	quiet       time.Duration // how long a stream must be silent to be suspected
	retry       time.Duration
	firstRetain time.Duration
	retain      time.Duration
}

var defaultTiming = timing{
	flush:       time.Second,
	scan:        5 * time.Second,
	report:      10 * time.Second,
	stuck:       30 * time.Second,
	quiet:       30 * time.Second,
	retry:       5 * time.Second,
	firstRetain: 30 * time.Second,
	retain:      time.Hour,
}

// Docker is the proxy, as the collector uses it.
type Docker interface {
	Containers(ctx context.Context) (containers.ContainersAnswer, error)
	Stream(ctx context.Context, id string, start containers.Start) (io.ReadCloser, error)
	Latest(ctx context.Context, id string) (time.Time, bool, error)
}

// Store is the database, as the collector writes it (blackbox_app).
type Store interface {
	Tracked(ctx context.Context) ([]store.Tracked, error)
	Register(ctx context.Context, c containers.Container, checkpoint int64, now time.Time) (store.Tracked, error)
	Save(ctx context.Context, b store.Batch, now time.Time) error
	SetDrained(ctx context.Context, id int32, at, started *time.Time) error
	SetRemoved(ctx context.Context, id int32, at time.Time) error
	SetDamaged(ctx context.Context, id int32, gap store.Gap) error
	AddGap(ctx context.Context, gap store.Gap) error
	LastHeartbeat(ctx context.Context) (time.Time, bool, error)
	Report(ctx context.Context, st store.CollectorStatus, streams []store.StreamReport, seen []int32) error
	Retain(ctx context.Context, now time.Time) (deleted, stored int64, err error)
	Close()
}

// Options are the collector's parts; tests swap them for fakes.
type Options struct {
	Version   string
	Docker    Docker
	OpenStore func(ctx context.Context) (Store, error) // retried until it works
	Heartbeat string                                   // HeartbeatFile unless set
	Log       *slog.Logger
	Now       func() time.Time // time.Now unless set
}

// Collector follows the containers' logs.
type Collector struct {
	opts   Options
	timing timing

	db      Store
	tracked map[string]*container
	events  chan event
	streams sync.WaitGroup
	runCtx  context.Context

	pending      store.Batch
	pendingBytes int
	pausedUntil  time.Time
	lastRead     *time.Time
	lastSaved    *time.Time
	lag          float64
	dbErr        string
	dockerErr    string
}

// container is one container being followed.
type container struct {
	store.Tracked
	info containers.Container

	stream       *stream // nil when none is open
	ended        bool    // its last stream ended
	endedClean   bool    // and reached the end of the log
	refused      bool    // the proxy is at its limit of streams
	missing      bool    // gone from Docker, its stream not yet ended
	lastDelivery time.Time
	suspect      int64 // the newest line time while the stream was silent

	state, lastError string
	reported         string
}

// stream is one open log stream.
type stream struct {
	cancel context.CancelFunc
}

// event is a line from a stream, or its end.
type event struct {
	c      *container
	stream *stream
	raw    lines.Raw
	rec    lines.Record
	opened bool // the stream is open

	skipped  bool // the stream jumped past lines it should have delivered
	from, to int64

	ended bool
	err   error
}

// New returns a collector.
func New(opts Options) *Collector {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Heartbeat == "" {
		opts.Heartbeat = HeartbeatFile
	}
	return &Collector{
		opts:    opts,
		timing:  defaultTiming,
		tracked: make(map[string]*container),
		events:  make(chan event, maxBatchLines),
	}
}

// Run collects until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) error {
	c.runCtx = ctx
	c.beat()
	if !c.connect(ctx) {
		return nil
	}
	defer c.db.Close()
	c.scan(ctx)

	flush := time.NewTicker(c.timing.flush)
	defer flush.Stop()
	scan := time.NewTicker(c.timing.scan)
	defer scan.Stop()
	report := time.NewTicker(c.timing.report)
	defer report.Stop()
	stuck := time.NewTicker(c.timing.stuck)
	defer stuck.Stop()
	retain := time.NewTimer(c.timing.firstRetain)
	defer retain.Stop()

	for {
		select {
		case <-ctx.Done():
			c.stop()
			return nil
		case ev := <-c.events:
			c.handle(ctx, ev)
		case <-flush.C:
			c.flush(ctx)
		case <-scan.C:
			c.scan(ctx)
		case <-report.C:
			c.report(ctx)
		case <-stuck.C:
			c.checkStuck(ctx)
		case <-retain.C:
			c.retain(ctx)
			retain.Reset(c.timing.retain)
		}
	}
}

// connect opens the database, migrating it, and picks up where the last run
// left off. It keeps trying until it works; false means ctx ended first.
func (c *Collector) connect(ctx context.Context) bool {
	for {
		err := c.open(ctx)
		if err == nil {
			return true
		}
		c.problem(&c.dbErr, "the database isn't ready: retrying", err)
		c.beat()
		select {
		case <-ctx.Done():
			return false
		case <-time.After(c.timing.retry):
		}
	}
}

func (c *Collector) open(ctx context.Context) error {
	db, err := c.opts.OpenStore(ctx)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	known, err := db.Tracked(callCtx)
	if err != nil {
		db.Close()
		return err
	}
	now := c.opts.Now()
	if last, ok, err := db.LastHeartbeat(callCtx); err != nil {
		db.Close()
		return err
	} else if ok && now.Sub(last) > downAfter {
		err := db.AddGap(callCtx, store.Gap{
			Kind: store.GapCollectorDown, From: last, To: now, NotedAt: now,
			Detail: "the collector wasn't running: lines were recovered unless a container was removed or its log rotated meanwhile",
		})
		if err != nil {
			db.Close()
			return err
		}
	}
	for _, t := range known {
		c.tracked[t.DockerID] = &container{Tracked: t}
	}
	c.db = db
	c.dbErr = ""
	c.opts.Log.Info("collector ready", "containers", len(known))
	return nil
}

// stop closes the streams and saves what's left.
func (c *Collector) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	for _, t := range c.tracked {
		c.close(t)
	}
	c.streams.Wait()
drain:
	for {
		select {
		case ev := <-c.events:
			if !ev.ended && !ev.opened && !ev.skipped {
				c.add(ev)
			}
		default:
			break drain
		}
	}
	c.flush(ctx)
	c.report(ctx)
}

// handle takes one event from a stream. Anything from a closed stream is
// dropped: its lines will be read again from the checkpoint.
func (c *Collector) handle(ctx context.Context, ev event) {
	if ev.c.stream != ev.stream {
		return
	}
	if ev.opened {
		ev.c.refused = false
		return
	}
	if ev.skipped {
		c.skipped(ctx, ev)
		return
	}
	if !ev.ended {
		c.add(ev)
		if len(c.pending.Records) >= maxBatchLines || c.pendingBytes >= maxBatchBytes {
			c.flush(ctx)
		}
		return
	}

	c.flush(ctx)
	t := ev.c
	if t.stream != ev.stream {
		return
	}
	t.stream.cancel()
	t.stream = nil
	t.ended, t.endedClean = true, ev.err == nil
	switch {
	case ev.err == nil:
		t.setState("ended", "")
	case errors.Is(ev.err, containers.ErrBusy):
		t.refused = true
		t.setState("refused", ev.err.Error())
	case errors.Is(ev.err, containers.ErrGone):
		t.setState("ended", "")
	default:
		if t.lastError != ev.err.Error() {
			c.opts.Log.Warn("a log stream failed", "container", t.info.Name, "err", ev.err)
		}
		t.setState("failed", ev.err.Error())
	}
}

// skipped records lines a stream jumped over. Docker does this at a damaged
// part of a log: with no error, it carries on with new lines.
func (c *Collector) skipped(ctx context.Context, ev event) {
	c.opts.Log.Warn("a log stream skipped lines, probably at a damaged part of the log", "container", ev.c.info.Name)
	now := c.opts.Now()
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callTimeout)
	defer cancel()
	err := c.db.SetDamaged(callCtx, ev.c.ID, store.Gap{
		Kind: store.GapLogDamaged, ContainerID: &ev.c.ID, From: time.Unix(0, ev.from), To: time.Unix(0, ev.to), NotedAt: now,
		Detail: damagedDetail(ev.c.info.Name),
	})
	if err != nil {
		c.databaseFailed(err)
		return
	}
	ev.c.Damaged = true
}

// damagedDetail explains a log_damaged gap.
func damagedDetail(name string) string {
	return fmt.Sprintf("%s's Docker log can't be read past a damaged part: lines in this stretch are lost, and it's read from its end until the container is replaced", name)
}

// add puts a line in the next batch.
func (c *Collector) add(ev event) {
	now := c.opts.Now()
	c.lastRead = &now
	ev.c.lastDelivery = now
	ev.c.suspect = 0

	at := ev.raw.Time
	if at.IsZero() {
		at = now
	} else {
		if c.pending.Checkpoints == nil {
			c.pending.Checkpoints = make(map[int32]int64)
		}
		c.pending.Checkpoints[ev.c.ID] = max(c.pending.Checkpoints[ev.c.ID], at.UnixNano())
	}
	c.pending.Records = append(c.pending.Records, store.Record{
		ContainerID: ev.c.ID, Time: at, Stream: ev.raw.Stream, Level: ev.rec.Level, Format: ev.rec.Format,
		Message: ev.rec.Message, Attrs: ev.rec.Attrs, Truncated: ev.rec.Truncated, Redacted: ev.rec.Redacted,
	})
	c.pendingBytes += len(ev.rec.Message) + len(ev.rec.Attrs)
}

// flush saves the batch. If it can't, every stream closes and reopens from its
// checkpoint once the database is back, so nothing is held here.
func (c *Collector) flush(ctx context.Context) {
	if len(c.pending.Records) == 0 {
		c.lag = 0
		return
	}
	now := c.opts.Now()
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callTimeout)
	defer cancel()
	err := c.db.Save(saveCtx, c.pending, now)
	if store.IsBadData(err) {
		c.opts.Log.Warn("Postgres refused a line's fields: saving this batch without them", "err", err)
		for i := range c.pending.Records {
			c.pending.Records[i].Attrs = nil
		}
		err = c.db.Save(saveCtx, c.pending, now)
	}
	if err != nil {
		c.problem(&c.dbErr, "saving failed: reopening the streams once the database answers", err)
		c.pending, c.pendingBytes = store.Batch{}, 0
		c.pause(now)
		return
	}

	c.clearProblem(&c.dbErr)
	c.lastSaved = &now
	c.lag = 0
	for _, t := range c.tracked {
		if ns, ok := c.pending.Checkpoints[t.ID]; ok {
			t.Checkpoint = max(t.Checkpoint, ns)
			c.lag = max(c.lag, now.Sub(time.Unix(0, ns)).Seconds())
		}
	}
	c.pending, c.pendingBytes = store.Batch{}, 0
}

// pause closes every stream until the retry time has passed.
func (c *Collector) pause(now time.Time) {
	c.pausedUntil = now.Add(c.timing.retry)
	for _, t := range c.tracked {
		c.close(t)
		t.suspect = 0
	}
}

func (c *Collector) paused() bool {
	return c.opts.Now().Before(c.pausedUntil)
}

// close ends a container's stream, if it has one, without marking it ended.
func (c *Collector) close(t *container) {
	if t.stream != nil {
		t.stream.cancel()
		t.stream = nil
	}
}

// scan compares Docker's containers with what's being followed.
func (c *Collector) scan(ctx context.Context) {
	if c.paused() {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*callTimeout)
	defer cancel()
	answer, err := c.opts.Docker.Containers(callCtx)
	if err != nil {
		if ctx.Err() == nil {
			c.problem(&c.dockerErr, "the proxy isn't answering: retrying", err)
		}
		return
	}
	c.clearProblem(&c.dockerErr)

	present := make(map[string]bool, len(answer.Containers)+len(answer.Failed))
	for _, f := range answer.Failed {
		present[f.ID] = true
	}
	for _, info := range answer.Containers {
		present[info.ID] = true
		if !c.visit(callCtx, info) {
			return
		}
	}
	for id, t := range c.tracked {
		if !present[id] && !c.removed(callCtx, t) {
			return
		}
	}
}

// visit brings one container up to date; false means the database failed.
func (c *Collector) visit(ctx context.Context, info containers.Container) bool {
	now := c.opts.Now()
	t := c.tracked[info.ID]
	if info.Excluded {
		if t != nil {
			c.close(t)
			t.setState("excluded", "")
		}
		return true
	}
	if t == nil {
		tr, err := c.register(ctx, info, now)
		if err != nil {
			c.databaseFailed(err)
			return false
		}
		t = &container{Tracked: tr}
		c.tracked[info.ID] = t
	}
	t.info = info
	t.missing = false

	if !sameMoment(t.StartedAt, info.StartedAt) {
		t.StartedAt = info.StartedAt
		t.Drained, t.ended = false, false
		if err := c.db.SetDrained(ctx, t.ID, nil, info.StartedAt); err != nil {
			c.databaseFailed(err)
			return false
		}
	}

	switch {
	case t.stream != nil:
	case info.LogDriver == "none":
		t.setState("no logs", "")
	case t.Drained:
		t.setState("read", "")
	case t.ended && t.endedClean && !info.Active():
		return c.drain(ctx, t)
	default:
		return c.follow(ctx, t)
	}
	return true
}

// register adds a new container, reading from its creation or the retention
// limit, whichever is later: older lines would only be deleted again.
func (c *Collector) register(ctx context.Context, info containers.Container, now time.Time) (store.Tracked, error) {
	from := now.Add(-store.Retention)
	if info.CreatedAt.After(from) {
		from = info.CreatedAt
	}
	return c.db.Register(ctx, info, from.UnixNano(), now)
}

// sameMoment compares two start times at the database's precision.
func sameMoment(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond))
}

// follow opens a container's stream, from its checkpoint unless its log is
// damaged: then only new lines can be read, and what's skipped is a gap.
func (c *Collector) follow(ctx context.Context, t *container) bool {
	now := c.opts.Now()
	start := containers.Start{SinceNanos: t.Checkpoint}
	after := t.Checkpoint
	if t.Damaged {
		start, after = containers.Start{FromEnd: true}, 0
		from := time.Unix(0, t.Checkpoint)
		if now.Sub(from) >= minGap {
			err := c.db.SetDamaged(ctx, t.ID, store.Gap{
				Kind: store.GapLogDamaged, ContainerID: &t.ID, From: from, To: now, NotedAt: now,
				Detail: damagedDetail(t.info.Name),
			})
			if err != nil {
				c.databaseFailed(err)
				return false
			}
			t.Checkpoint = now.UnixNano()
		}
	}
	if t.lastDelivery.IsZero() {
		t.lastDelivery = now
	}

	streamCtx, cancel := context.WithCancel(c.runCtx)
	s := &stream{cancel: cancel}
	t.stream, t.ended = s, false
	t.setState("open", "")
	c.streams.Add(1)
	go c.read(streamCtx, t, s, t.info.ID, t.info.TTY, start, after)
	return true
}

// read follows one stream, parsing lines and handing them to the loop.
func (c *Collector) read(ctx context.Context, t *container, s *stream, id string, tty bool, start containers.Start, after int64) {
	defer c.streams.Done()
	send := func(ev event) bool {
		ev.c, ev.stream = t, s
		select {
		case c.events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	// The newest line now must arrive before any later one. If a later one
	// comes first, Docker jumped over the backlog.
	var expected int64
	if !start.FromEnd {
		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		if latest, ok, err := c.opts.Docker.Latest(callCtx, id); err == nil && ok && latest.UnixNano() > after {
			expected = latest.UnixNano()
		}
		cancel()
	}
	last := after

	body, err := c.opts.Docker.Stream(ctx, id, start)
	if err != nil {
		send(event{ended: true, err: err})
		return
	}
	defer body.Close()
	if !send(event{opened: true}) {
		return
	}

	r := lines.NewReader(body, tty)
	for {
		raw, err := r.Next()
		if errors.Is(err, io.EOF) {
			send(event{ended: true})
			return
		}
		if err != nil {
			send(event{ended: true, err: err})
			return
		}
		if !raw.Time.IsZero() {
			ns := raw.Time.UnixNano()
			if ns <= after {
				continue
			}
			if expected != 0 && ns > expected+orderSlack {
				if !send(event{skipped: true, from: last, to: ns}) {
					return
				}
				expected = 0
			} else if ns >= expected {
				expected = 0
			}
			last = ns
		}
		if !send(event{raw: raw, rec: lines.Parse(raw.Text)}) {
			return
		}
	}
}

// drain finishes a stopped container. A stream can stop short of the end at a
// damaged part of the log, so its newest line is checked first.
func (c *Collector) drain(ctx context.Context, t *container) bool {
	now := c.opts.Now()
	if latest, ok, err := c.opts.Docker.Latest(ctx, t.info.ID); err == nil && ok && latest.UnixNano() > t.Checkpoint {
		err := c.db.AddGap(ctx, store.Gap{
			Kind: store.GapLogDamaged, ContainerID: &t.ID, From: time.Unix(0, t.Checkpoint), To: latest, NotedAt: now,
			Detail: fmt.Sprintf("%s's Docker log couldn't be read to its end", t.info.Name),
		})
		if err != nil {
			c.databaseFailed(err)
			return false
		}
	}
	if err := c.db.SetDrained(ctx, t.ID, &now, t.StartedAt); err != nil {
		c.databaseFailed(err)
		return false
	}
	t.Drained = true
	t.setState("read", "")
	return true
}

// removed handles a container gone from Docker. Its stream gets one scan to
// end by itself; a container that wasn't read to the end leaves a gap.
func (c *Collector) removed(ctx context.Context, t *container) bool {
	if t.stream != nil && !t.missing {
		t.missing = true
		return true
	}
	c.close(t)
	now := c.opts.Now()
	if !t.Drained && !(t.ended && t.endedClean) {
		err := c.db.AddGap(ctx, store.Gap{
			Kind: store.GapContainerRemoved, ContainerID: &t.ID, From: time.Unix(0, t.Checkpoint), To: now, NotedAt: now,
			Detail: "the container was removed before its log was read to the end: lines from this time on may be lost",
		})
		if err != nil {
			c.databaseFailed(err)
			return false
		}
	}
	if err := c.db.SetRemoved(ctx, t.ID, now); err != nil {
		c.databaseFailed(err)
		return false
	}
	delete(c.tracked, t.DockerID)
	return true
}

// checkStuck finds streams that went silent while their container kept
// logging, which is what a damaged log looks like. A stream is only given up
// on when it's still silent, with newer lines waiting, on the next check.
func (c *Collector) checkStuck(ctx context.Context) {
	if c.paused() {
		return
	}
	now := c.opts.Now()
	for _, t := range c.tracked {
		if !t.info.Active() || t.info.Excluded || t.refused || now.Sub(t.lastDelivery) < c.timing.quiet {
			t.suspect = 0
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		latest, ok, err := c.opts.Docker.Latest(callCtx, t.info.ID)
		cancel()
		if err != nil || !ok || latest.UnixNano() <= t.Checkpoint {
			t.suspect = 0
			continue
		}
		if t.suspect == 0 {
			t.suspect = latest.UnixNano()
			continue
		}
		c.opts.Log.Warn("a log stream is stuck, probably at a damaged part of the log: following new lines only", "container", t.info.Name)
		t.suspect, t.Damaged = 0, true
		c.close(t)
		callCtx, cancel = context.WithTimeout(ctx, callTimeout)
		c.follow(callCtx, t)
		cancel()
	}
}

// report saves the collector's status and refreshes the heartbeat.
func (c *Collector) report(ctx context.Context) {
	c.beat()
	now := c.opts.Now()
	st := store.CollectorStatus{
		HeartbeatAt: now, Version: c.opts.Version, LastReadAt: c.lastRead, LastPersistedAt: c.lastSaved,
		LagSeconds: c.lag, LastError: cmp.Or(c.dbErr, c.dockerErr),
	}
	var streams []store.StreamReport
	seen := make([]int32, 0, len(c.tracked))
	for _, t := range c.tracked {
		seen = append(seen, t.ID)
		if t.stream != nil {
			st.StreamsOpen++
		}
		if t.refused {
			st.StreamsRefused++
		}
		if now := t.state + "\x00" + t.lastError; now != t.reported {
			streams = append(streams, store.StreamReport{ID: t.ID, State: t.state, LastError: t.lastError})
			t.reported = now
		}
	}
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callTimeout)
	defer cancel()
	if err := c.db.Report(callCtx, st, streams, seen); err != nil {
		for _, t := range c.tracked {
			t.reported = ""
		}
		c.problem(&c.dbErr, "saving the status failed", err)
	}
}

// retain deletes what's past retention.
func (c *Collector) retain(ctx context.Context) {
	callCtx, cancel := context.WithTimeout(ctx, retainTimeout)
	defer cancel()
	deleted, stored, err := c.db.Retain(callCtx, c.opts.Now())
	if err != nil {
		c.problem(&c.dbErr, "retention failed", err)
		return
	}
	if deleted > 0 {
		c.opts.Log.Info("old records deleted", "records", deleted, "stored_bytes", stored)
	}
}

// databaseFailed stops reading until the database answers again.
func (c *Collector) databaseFailed(err error) {
	c.problem(&c.dbErr, "the database failed: reopening the streams once it answers", err)
	c.pause(c.opts.Now())
}

// problem logs a new problem once and keeps it in slot for `status`. Each
// source has its own slot, so Docker answering doesn't clear a database error.
func (c *Collector) problem(slot *string, msg string, err error) {
	text := err.Error()
	if text != *slot {
		c.opts.Log.Error(msg, "err", err)
	}
	*slot = text
}

func (c *Collector) clearProblem(slot *string) {
	if *slot != "" {
		c.opts.Log.Info("working again")
		*slot = ""
	}
}

func (t *container) setState(state, lastError string) {
	t.state, t.lastError = state, lastError
}

// beat refreshes the heartbeat file. A failure shows up as an unhealthy
// container, and the log says why.
func (c *Collector) beat() {
	stamp := []byte(c.opts.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(c.opts.Heartbeat, stamp, 0o644); err != nil {
		c.opts.Log.Error("writing the heartbeat failed: is the tmpfs mounted at its folder?", "err", err)
	}
}

// Healthy returns nil while the heartbeat at path is fresh. It doesn't depend
// on Docker or the database, so a sparkdb restart doesn't roll Blackbox back.
func Healthy(path string, now time.Time) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("no heartbeat: is blackbox-collector running? %w", err)
	}
	at, err := time.Parse(time.RFC3339, string(data))
	if err != nil {
		return fmt.Errorf("the heartbeat is unreadable: %w", err)
	}
	if age := now.Sub(at); age > heartbeatLimit {
		return fmt.Errorf("the heartbeat is %s old: is blackbox-collector stuck?", age.Round(time.Second))
	}
	return nil
}

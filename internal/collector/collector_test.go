package collector

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsariol/blackbox/internal/containers"
	"github.com/lsariol/blackbox/internal/store"
)

// fakeDocker is the proxy: containers, each with a log that can grow.
type fakeDocker struct {
	mu         sync.Mutex
	list       []containers.Container
	logs       map[string][]logLine
	damagedAt  map[string]int // reading from the start stops at this line
	listErr    error
	latestErr  error
	streamErr  error
	opens      []containers.Start
	changed    chan struct{}
	silentStop bool // a damaged stream stays open and silent, rather than ending
	jumpOver   bool // a damaged stream carries on with new lines, as Docker really does
}

type logLine struct {
	at   time.Time
	text string
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{logs: map[string][]logLine{}, damagedAt: map[string]int{}, changed: make(chan struct{})}
}

func (f *fakeDocker) notify() {
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *fakeDocker) add(c containers.Container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = append(f.list, c)
	f.notify()
}

func (f *fakeDocker) write(id string, at time.Time, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs[id] = append(f.logs[id], logLine{at, text})
	f.notify()
}

func (f *fakeDocker) set(id string, edit func(*containers.Container)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.list {
		if f.list[i].ID == id {
			edit(&f.list[i])
		}
	}
	f.notify()
}

func (f *fakeDocker) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.list {
		if f.list[i].ID == id {
			f.list = append(f.list[:i], f.list[i+1:]...)
			break
		}
	}
	f.notify()
}

func (f *fakeDocker) setErr(listErr, streamErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listErr, f.streamErr = listErr, streamErr
}

func (f *fakeDocker) starts() []containers.Start {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]containers.Start(nil), f.opens...)
}

func (f *fakeDocker) Containers(ctx context.Context) (containers.ContainersAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return containers.ContainersAnswer{}, f.listErr
	}
	return containers.ContainersAnswer{Containers: append([]containers.Container(nil), f.list...)}, nil
}

func (f *fakeDocker) Latest(ctx context.Context, id string) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.latestErr != nil {
		return time.Time{}, false, f.latestErr
	}
	l := f.logs[id]
	if len(l) == 0 {
		return time.Time{}, false, nil
	}
	return l[len(l)-1].at, true, nil
}

func (f *fakeDocker) Stream(ctx context.Context, id string, start containers.Start) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens = append(f.opens, start)
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	pr, pw := io.Pipe()
	go f.feed(ctx, pw, id, start)
	go func() {
		<-ctx.Done()
		pr.CloseWithError(ctx.Err())
	}()
	return pr, nil
}

// feed writes lines from start on, following while the container runs.
func (f *fakeDocker) feed(ctx context.Context, w *io.PipeWriter, id string, start containers.Start) {
	f.mu.Lock()
	next := 0
	if start.FromEnd {
		next = len(f.logs[id])
	}
	f.mu.Unlock()
	for {
		f.mu.Lock()
		damage, damaged := f.damagedAt[id]
		if damaged && !start.FromEnd && next >= damage && f.jumpOver {
			next = len(f.logs[id])
			delete(f.damagedAt, id)
			f.mu.Unlock()
			continue
		}
		if damaged && !start.FromEnd && next >= damage {
			silent := f.silentStop
			f.mu.Unlock()
			if silent {
				<-ctx.Done()
			}
			w.Close()
			return
		}
		var line *logLine
		if next < len(f.logs[id]) {
			line = &f.logs[id][next]
			next++
		}
		running := false
		for _, c := range f.list {
			if c.ID == id && c.Active() {
				running = true
			}
		}
		changed := f.changed
		f.mu.Unlock()

		switch {
		case line != nil:
			if !start.FromEnd && line.at.UnixNano() < start.SinceNanos {
				continue
			}
			text := line.at.UTC().Format("2006-01-02T15:04:05.000000000Z") + " " + line.text + "\n"
			head := make([]byte, 8)
			head[0] = 2
			binary.BigEndian.PutUint32(head[4:], uint32(len(text)))
			if _, err := w.Write(append(head, text...)); err != nil {
				return
			}
		case !running:
			w.Close()
			return
		default:
			select {
			case <-changed:
			case <-ctx.Done():
				w.CloseWithError(ctx.Err())
				return
			}
		}
	}
}

// fakeStore keeps what the collector saves.
type fakeStore struct {
	mu        sync.Mutex
	nextID    int32
	rows      map[string]*store.Tracked
	records   []store.Record
	gaps      []store.Gap
	failSave  bool
	badAttrs  bool
	heartbeat *time.Time
	removed   map[int32]bool
	reports   int
	retained  int
	status    store.CollectorStatus
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]*store.Tracked{}, removed: map[int32]bool{}}
}

func (s *fakeStore) Tracked(ctx context.Context) ([]store.Tracked, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Tracked
	for _, t := range s.rows {
		if !s.removed[t.ID] {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (s *fakeStore) Register(ctx context.Context, c containers.Container, checkpoint int64, now time.Time) (store.Tracked, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.rows[c.ID]; ok {
		return *t, nil
	}
	s.nextID++
	t := &store.Tracked{ID: s.nextID, DockerID: c.ID, Checkpoint: checkpoint, StartedAt: c.StartedAt}
	s.rows[c.ID] = t
	return *t, nil
}

func (s *fakeStore) Save(ctx context.Context, b store.Batch, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSave {
		return errors.New("database: down")
	}
	s.records = append(s.records, b.Records...)
	for _, t := range s.rows {
		if ns, ok := b.Checkpoints[t.ID]; ok && ns > t.Checkpoint {
			t.Checkpoint = ns
		}
	}
	return nil
}

func (s *fakeStore) row(id int32) *store.Tracked {
	for _, t := range s.rows {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (s *fakeStore) SetDrained(ctx context.Context, id int32, at, started *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.row(id).Drained, s.row(id).StartedAt = at != nil, started
	return nil
}

func (s *fakeStore) SetRemoved(ctx context.Context, id int32, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed[id] = true
	return nil
}

func (s *fakeStore) SetDamaged(ctx context.Context, id int32, gap store.Gap) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.row(id)
	r.Damaged = true
	r.Checkpoint = gap.To.UnixNano()
	s.gaps = append(s.gaps, gap)
	return nil
}

func (s *fakeStore) AddGap(ctx context.Context, gap store.Gap) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gaps = append(s.gaps, gap)
	return nil
}

func (s *fakeStore) LastHeartbeat(ctx context.Context) (time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.heartbeat == nil {
		return time.Time{}, false, nil
	}
	return *s.heartbeat, true, nil
}

func (s *fakeStore) Report(ctx context.Context, st store.CollectorStatus, streams []store.StreamReport, seen []int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports++
	s.status = st
	s.heartbeat = &st.HeartbeatAt
	return nil
}

func (s *fakeStore) Retain(ctx context.Context, now time.Time) (int64, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retained++
	return 0, 0, nil
}

func (s *fakeStore) Close() {}

func (s *fakeStore) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.records {
		out = append(out, r.Message)
	}
	return out
}

func (s *fakeStore) gapKinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, g := range s.gaps {
		out = append(out, g.Kind)
	}
	return out
}

func (s *fakeStore) setFail(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failSave = fail
}

var (
	quiet = slog.New(slog.NewTextHandler(io.Discard, nil))
	t0    = time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
)

func running(id string) containers.Container {
	started := t0
	return containers.Container{ID: id, Name: id, Project: id, Service: id, State: "running", LogDriver: "json-file", CreatedAt: t0.Add(-time.Minute), StartedAt: &started}
}

// run starts a collector with short intervals and stops it at the test's end.
func run(t *testing.T, d *fakeDocker, s *fakeStore) (stop func()) {
	t.Helper()
	c := New(Options{
		Version:   "test",
		Docker:    d,
		OpenStore: func(context.Context) (Store, error) { return s, nil },
		Heartbeat: filepath.Join(t.TempDir(), "heartbeat"),
		Log:       quiet,
	})
	c.timing = timing{
		flush: 5 * time.Millisecond, scan: 10 * time.Millisecond, report: 20 * time.Millisecond,
		stuck: 20 * time.Millisecond, quiet: 40 * time.Millisecond, retry: 20 * time.Millisecond,
		firstRetain: 30 * time.Millisecond, retain: time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := c.Run(ctx); err != nil {
			t.Error(err)
		}
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	return stop
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}

func waitFor(t *testing.T, s *fakeStore, want ...string) {
	t.Helper()
	eventually(t, "messages "+strings.Join(want, ","), func() bool {
		return strings.Join(s.messages(), ",") == strings.Join(want, ",")
	})
}

func TestCollectsAndFollows(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	d.add(running("lys"))
	d.write("lys", t0.Add(time.Second), "one")
	d.write("lys", t0.Add(2*time.Second), "two")
	run(t, d, s)

	waitFor(t, s, "one", "two")
	d.write("lys", t0.Add(3*time.Second), "three")
	waitFor(t, s, "one", "two", "three")

	eventually(t, "the checkpoint", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.rows["lys"].Checkpoint == t0.Add(3*time.Second).UnixNano()
	})
	eventually(t, "a report and retention", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.reports > 0 && s.retained > 0 && s.status.StreamsOpen == 1 && s.status.Version == "test"
	})
}

func TestParsesAndRedactsLines(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	d.add(running("lys"))
	d.write("lys", t0.Add(time.Second), `{"level":"ERROR","msg":"boom token=abc123","operation":"x"}`)
	run(t, d, s)

	eventually(t, "the line", func() bool { return len(s.messages()) == 1 })
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.records[0]
	if r.Level != 40 || r.Format != 1 || r.Message != "boom token=[redacted]" || !r.Redacted || r.Stream != 2 || string(r.Attrs) != `{"operation":"x"}` {
		t.Errorf("record = %+v", r)
	}
}

func TestResumesWithoutDuplicates(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	d.add(running("lys"))
	d.write("lys", t0.Add(time.Second), "one")
	d.write("lys", t0.Add(2*time.Second), "two")
	stop := run(t, d, s)
	waitFor(t, s, "one", "two")
	stop()

	d.write("lys", t0.Add(3*time.Second), "three")
	run(t, d, s)
	waitFor(t, s, "one", "two", "three")
	time.Sleep(30 * time.Millisecond)
	if got := s.messages(); len(got) != 3 {
		t.Errorf("messages = %v", got)
	}
	starts := d.starts()
	if last := starts[len(starts)-1]; last.SinceNanos != t0.Add(2*time.Second).UnixNano() {
		t.Errorf("resumed from %d", last.SinceNanos)
	}
}

func TestDatabaseOutageLosesNothing(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	d.add(running("lys"))
	d.write("lys", t0.Add(time.Second), "one")
	run(t, d, s)
	waitFor(t, s, "one")

	s.setFail(true)
	d.write("lys", t0.Add(2*time.Second), "two")
	d.write("lys", t0.Add(3*time.Second), "three")
	eventually(t, "the streams to reopen and fail again", func() bool { return len(d.starts()) >= 3 })
	s.setFail(false)

	waitFor(t, s, "one", "two", "three")
}

func TestProxyOutageResumes(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	d.add(running("lys"))
	d.write("lys", t0.Add(time.Second), "one")
	d.setErr(errors.New("proxy: down"), nil)
	run(t, d, s)
	time.Sleep(30 * time.Millisecond)
	if got := s.messages(); len(got) != 0 {
		t.Fatalf("messages = %v", got)
	}
	d.setErr(nil, nil)
	waitFor(t, s, "one")
}

func TestStoppedContainerIsDrainedOnce(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	stopped := running("job")
	stopped.State = "exited"
	d.add(stopped)
	d.write("job", t0.Add(time.Second), "finished")
	run(t, d, s)

	waitFor(t, s, "finished")
	eventually(t, "drained", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		r := s.rows["job"]
		return r != nil && r.Drained
	})
	opens := len(d.starts())
	time.Sleep(40 * time.Millisecond)
	if len(d.starts()) != opens {
		t.Errorf("a drained container was read again: %d opens, then %d", opens, len(d.starts()))
	}
	if gaps := s.gapKinds(); len(gaps) != 0 {
		t.Errorf("gaps = %v", gaps)
	}
}

func TestRestartedContainerIsReadAgain(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	stopped := running("job")
	stopped.State = "exited"
	d.add(stopped)
	d.write("job", t0.Add(time.Second), "first run")
	run(t, d, s)
	eventually(t, "drained", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		r := s.rows["job"]
		return r != nil && r.Drained
	})

	later := t0.Add(time.Minute)
	d.write("job", t0.Add(2*time.Minute), "second run")
	d.set("job", func(c *containers.Container) { c.State, c.StartedAt = "running", &later })
	waitFor(t, s, "first run", "second run")
}

func TestDrainedContainerStaysDrainedAcrossRestarts(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	stopped := running("job")
	started := t0.Add(123456789 * time.Nanosecond)
	stopped.StartedAt, stopped.State = &started, "exited"
	d.add(stopped)
	d.write("job", t0.Add(time.Second), "done")
	stop := run(t, d, s)
	eventually(t, "drained", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		r := s.rows["job"]
		return r != nil && r.Drained
	})
	stop()

	// The database keeps microseconds; this mustn't look like a restart.
	s.mu.Lock()
	rounded := started.Truncate(time.Microsecond)
	s.rows["job"].StartedAt = &rounded
	s.mu.Unlock()
	opens := len(d.starts())
	run(t, d, s)
	time.Sleep(40 * time.Millisecond)
	if len(d.starts()) != opens {
		t.Errorf("a drained container was read again after a restart")
	}
}

func TestContainerRemovedAfterBeingReadLeavesNoGap(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	d.add(running("old"))
	d.write("old", t0.Add(time.Second), "last line")
	run(t, d, s)
	waitFor(t, s, "last line")

	d.remove("old")
	eventually(t, "removed", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.removed[1]
	})
	if gaps := s.gapKinds(); len(gaps) != 0 {
		t.Errorf("gaps = %v", gaps)
	}
}

func TestContainerRemovedWhileCollectorWasDownLeavesAGap(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	d.add(running("old"))
	d.write("old", t0.Add(time.Second), "seen")
	stop := run(t, d, s)
	waitFor(t, s, "seen")
	stop()

	d.remove("old")
	d.add(running("new"))
	d.write("new", t0.Add(2*time.Second), "replacement")
	run(t, d, s)
	waitFor(t, s, "seen", "replacement")
	eventually(t, "a gap", func() bool {
		kinds := s.gapKinds()
		return len(kinds) == 1 && kinds[0] == store.GapContainerRemoved
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.gaps[0].From.Equal(t0.Add(time.Second)) {
		t.Errorf("gap starts at %v", s.gaps[0].From)
	}
}

func TestCollectorDownIsRecorded(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	last := time.Now().Add(-5 * time.Minute)
	s.heartbeat = &last
	run(t, d, s)
	eventually(t, "a gap", func() bool {
		kinds := s.gapKinds()
		return len(kinds) == 1 && kinds[0] == store.GapCollectorDown
	})
}

func TestShortRestartIsNotAGap(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	last := time.Now().Add(-10 * time.Second)
	s.heartbeat = &last
	run(t, d, s)
	time.Sleep(30 * time.Millisecond)
	if gaps := s.gapKinds(); len(gaps) != 0 {
		t.Errorf("gaps = %v", gaps)
	}
}

func testDamage(t *testing.T, silent bool) {
	d, s := newFakeDocker(), newFakeStore()
	d.silentStop = silent
	d.add(running("web"))
	d.write("web", t0.Add(time.Second), "before the damage")
	d.damagedAt["web"] = 1
	d.write("web", t0.Add(2*time.Second), "after the damage, unreadable from the start")
	run(t, d, s)

	waitFor(t, s, "before the damage")
	eventually(t, "the damage to be noticed", func() bool {
		kinds := s.gapKinds()
		return len(kinds) == 1 && kinds[0] == store.GapLogDamaged
	})
	eventually(t, "a stream from the end", func() bool {
		starts := d.starts()
		return starts[len(starts)-1].FromEnd
	})
	time.Sleep(10 * time.Millisecond)
	d.write("web", t0.Add(3*time.Second), "new line")
	waitFor(t, s, "before the damage", "new line")
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.rows["web"].Damaged {
		t.Error("the container should stay marked damaged")
	}
}

func TestDamagedLogWhereTheStreamEnds(t *testing.T) { testDamage(t, false) }

func TestDamagedLogWhereTheStreamGoesSilent(t *testing.T) { testDamage(t, true) }

func TestDamagedLogStaysDamagedAcrossRestarts(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	d.add(running("web"))
	s.rows["web"] = &store.Tracked{ID: 1, DockerID: "web", Checkpoint: t0.UnixNano(), Damaged: true, StartedAt: running("web").StartedAt}
	s.nextID = 1
	run(t, d, s)
	eventually(t, "a stream from the end", func() bool {
		starts := d.starts()
		return len(starts) > 0 && starts[0].FromEnd
	})
	if got := s.gapKinds(); len(got) != 1 || got[0] != store.GapLogDamaged {
		t.Errorf("gaps = %v", got)
	}
}

func TestQuietContainerIsNotStuck(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	d.add(running("quiet"))
	d.write("quiet", t0.Add(time.Second), "only line")
	run(t, d, s)
	waitFor(t, s, "only line")
	time.Sleep(150 * time.Millisecond)
	if gaps := s.gapKinds(); len(gaps) != 0 {
		t.Errorf("gaps = %v", gaps)
	}
	if starts := d.starts(); len(starts) != 1 || starts[0].FromEnd {
		t.Errorf("starts = %+v", starts)
	}
}

func TestStoppedContainerWithDamagedLogLeavesAGap(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	stopped := running("web")
	stopped.State = "exited"
	d.add(stopped)
	d.write("web", t0.Add(time.Second), "before")
	d.damagedAt["web"] = 1
	d.write("web", t0.Add(2*time.Second), "after")
	run(t, d, s)
	waitFor(t, s, "before")
	eventually(t, "a gap", func() bool {
		kinds := s.gapKinds()
		return len(kinds) == 1 && kinds[0] == store.GapLogDamaged
	})
}

func TestExcludedContainerIsSkipped(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	c := running("private")
	c.Excluded = true
	d.add(c)
	d.write("private", t0.Add(time.Second), "secret")
	run(t, d, s)
	time.Sleep(40 * time.Millisecond)
	if len(s.messages()) != 0 || len(d.starts()) != 0 {
		t.Errorf("messages %v, starts %v", s.messages(), d.starts())
	}
}

func TestContainerWithoutLogsIsSkipped(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	c := running("silent")
	c.LogDriver = "none"
	d.add(c)
	run(t, d, s)
	time.Sleep(30 * time.Millisecond)
	if len(d.starts()) != 0 {
		t.Errorf("starts = %v", d.starts())
	}
}

func TestBusyProxyIsRetriedAndNotCalledDamage(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	d.add(running("lys"))
	d.write("lys", t0.Add(time.Second), "one")
	d.setErr(nil, containers.ErrBusy)
	run(t, d, s)
	eventually(t, "retries", func() bool { return len(d.starts()) >= 3 })
	d.setErr(nil, nil)
	waitFor(t, s, "one")
	if gaps := s.gapKinds(); len(gaps) != 0 {
		t.Errorf("gaps = %v", gaps)
	}
}

func TestFirstReadStartsAtCreationOrRetention(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	young := running("young")
	old := running("old")
	old.CreatedAt = time.Now().Add(-90 * 24 * time.Hour)
	d.add(young)
	d.add(old)
	run(t, d, s)
	eventually(t, "both registered", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.rows) == 2
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if got := s.rows["young"].Checkpoint; got != young.CreatedAt.UnixNano() {
		t.Errorf("young starts at %d", got)
	}
	limit := time.Now().Add(-store.Retention - time.Minute).UnixNano()
	if got := s.rows["old"].Checkpoint; got < limit {
		t.Errorf("old starts past the retention limit")
	}
}

func TestHealthy(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	path := filepath.Join(t.TempDir(), "heartbeat")
	if err := Healthy(path, time.Now()); err == nil {
		t.Error("no file should be unhealthy")
	}
	c := New(Options{Docker: d, OpenStore: func(context.Context) (Store, error) { return s, nil }, Heartbeat: path, Log: quiet})
	c.beat()
	if err := Healthy(path, time.Now()); err != nil {
		t.Error(err)
	}
	if err := Healthy(path, time.Now().Add(time.Minute)); err == nil {
		t.Error("an old heartbeat should be unhealthy")
	}
}

func TestLongBusySpellIsNeverCalledDamage(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	d.add(running("lys"))
	d.write("lys", t0.Add(time.Second), "one")
	d.setErr(nil, containers.ErrBusy)
	run(t, d, s)
	time.Sleep(300 * time.Millisecond)
	if gaps := s.gapKinds(); len(gaps) != 0 {
		t.Errorf("gaps = %v", gaps)
	}
	for _, start := range d.starts() {
		if start.FromEnd {
			t.Fatal("a busy proxy must not count as a damaged log")
		}
	}
}

func TestSkippedBacklogIsRecorded(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	d.jumpOver = true
	d.add(running("web"))
	d.write("web", t0.Add(time.Second), "before the damage")
	stop := run(t, d, s)
	waitFor(t, s, "before the damage")
	stop()

	d.mu.Lock()
	d.damagedAt["web"] = 1
	d.mu.Unlock()
	d.write("web", t0.Add(2*time.Second), "skipped by Docker")
	run(t, d, s)
	eventually(t, "the stream to open", func() bool { return len(d.starts()) >= 2 })
	time.Sleep(20 * time.Millisecond)
	d.write("web", t0.Add(3*time.Second), "new line")

	waitFor(t, s, "before the damage", "new line")
	eventually(t, "a gap", func() bool {
		kinds := s.gapKinds()
		return len(kinds) == 1 && kinds[0] == store.GapLogDamaged
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.gaps[0]
	if !g.From.Equal(t0.Add(time.Second)) || !g.To.Equal(t0.Add(3*time.Second)) {
		t.Errorf("gap = %v to %v", g.From, g.To)
	}
	if !s.rows["web"].Damaged {
		t.Error("the container should be marked damaged")
	}
}

func TestNormalBacklogIsNotASkip(t *testing.T) {
	d, s := newFakeDocker(), newFakeStore()
	d.add(running("web"))
	d.write("web", t0.Add(time.Second), "one")
	stop := run(t, d, s)
	waitFor(t, s, "one")
	stop()
	d.write("web", t0.Add(2*time.Second), "two")
	d.write("web", t0.Add(3*time.Second), "three")
	run(t, d, s)
	waitFor(t, s, "one", "two", "three")
	d.write("web", t0.Add(4*time.Second), "four")
	waitFor(t, s, "one", "two", "three", "four")
	if gaps := s.gapKinds(); len(gaps) != 0 {
		t.Errorf("gaps = %v", gaps)
	}
}

func TestDockerAnsweringDoesNotClearADatabaseError(t *testing.T) {
	c := New(Options{Log: quiet})
	c.dbErr = "database: down"
	c.clearProblem(&c.dockerErr)
	if c.dbErr == "" {
		t.Error("a proxy success cleared the database's error")
	}
}

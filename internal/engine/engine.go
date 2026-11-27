package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"browsercompute.local/runtime/internal/protocol"
	"browsercompute.local/runtime/internal/workload"
)

const (
	MaxWorkers       = 32
	maxJobs          = 16
	maxBytes         = 64 << 20
	maxAttempts      = 3
	outboundSize     = 16
	heartbeat        = 2 * time.Second
	heartbeatTimeout = 8 * time.Second
	taskTimeout      = 10 * time.Second
	jobTimeout       = 5 * time.Minute
	retention        = 30 * time.Minute
)

var (
	ErrMissing  = errors.New("job not found")
	ErrConflict = errors.New("job already active or invalid job state")
	ErrCapacity = errors.New("coordinator capacity reached")
	ErrClosed   = errors.New("coordinator closed")
)

type Status struct {
	ID       string         `json:"id"`
	State    string         `json:"state"`
	Input    workload.Input `json:"input"`
	Total    int            `json:"total"`
	Accepted int            `json:"accepted"`
	Retries  int            `json:"retries"`
	Elapsed  int64          `json:"elapsed_ms"`
	Reason   string         `json:"reason,omitempty"`
}

type WorkerStatus struct {
	ID     string `json:"session_id"`
	State  string `json:"state"`
	Slots  int    `json:"slots"`
	Active int    `json:"active"`
}

type Page struct {
	Tiles []workload.Tile `json:"tiles"`
	Next  int             `json:"next_cursor"`
}

type Session struct {
	ID   string
	Out  <-chan protocol.Out
	Done <-chan struct{}
}

type worker struct {
	WorkerStatus
	seen time.Time
	free int
	out  chan protocol.Out
	done chan struct{}
}

type task struct {
	rect    workload.Rect
	attempt int
}

type lease struct {
	protocol.Lease
	owner *worker
	task  *task
	until time.Time
}

type job struct {
	Status
	created, deadline, ended time.Time
	bytes                    int
	tasks                    []task
	ready                    []int
	tiles                    []workload.Tile
}

type state struct {
	now     time.Time
	id      func() string
	jobs    map[string]*job
	workers []*worker
	lost    []WorkerStatus
	active  *job
	live    map[string]*lease
	bytes   int
}

type Engine struct {
	commands chan func(*state)
	done     chan struct{}
}

func New(ctx context.Context, clock func() time.Time, id func() string) *Engine {
	if clock == nil {
		clock = time.Now
	}
	if id == nil {
		id = func() string {
			var b [16]byte
			rand.Read(b[:])
			return hex.EncodeToString(b[:])
		}
	}
	e := &Engine{make(chan func(*state), 64), make(chan struct{})}
	go func() {
		s := &state{id: id, jobs: make(map[string]*job), live: make(map[string]*lease)}
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		defer close(e.done)
		defer func() {
			for _, w := range s.workers {
				close(w.done)
			}
		}()
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case command := <-e.commands:
				s.now = clock()
				s.expire()
				command(s)
				s.dispatch()
			case <-tick.C:
				s.now = clock()
				s.expire()
				s.dispatch()
			}
		}
	}()
	return e
}

func (e *Engine) Done() <-chan struct{} { return e.done }

type reply[T any] struct {
	value T
	err   error
}

func call[T any](ctx context.Context, e *Engine, fn func(*state) (T, error)) (T, error) {
	var zero T
	result := make(chan reply[T], 1)
	select {
	case <-e.done:
		return zero, ErrClosed
	case <-ctx.Done():
		return zero, ctx.Err()
	case e.commands <- func(s *state) {
		if ctx.Err() != nil {
			return
		}
		v, err := fn(s)
		result <- reply[T]{v, err}
	}:
	}
	select {
	case <-e.done:
		return zero, ErrClosed
	case <-ctx.Done():
		return zero, ctx.Err()
	case r := <-result:
		return r.value, r.err
	}
}

func (e *Engine) Submit(ctx context.Context, in workload.Input) (Status, error) {
	if err := in.Validate(); err != nil {
		return Status{}, err
	}
	rects := in.Partition()
	return call(ctx, e, func(s *state) (Status, error) {
		if s.active != nil {
			return Status{}, ErrConflict
		}
		size := 2 * in.Width * in.Height
		if len(s.jobs) >= maxJobs || s.bytes+size > maxBytes {
			return Status{}, ErrCapacity
		}
		j := &job{
			Status:  Status{ID: s.id(), State: "QUEUED", Input: in, Total: len(rects)},
			created: s.now, deadline: s.now.Add(jobTimeout), bytes: size,
			tasks: make([]task, len(rects)), ready: make([]int, len(rects)),
		}
		for i, r := range rects {
			j.tasks[i].rect = r
			j.ready[i] = i
		}
		s.bytes += size
		s.jobs[j.ID], s.active = j, j
		return j.Status, nil
	})
}

func (j *job) status(now time.Time) Status {
	v := j.Status
	if !j.ended.IsZero() {
		now = j.ended
	}
	v.Elapsed = now.Sub(j.created).Milliseconds()
	return v
}

func (e *Engine) Status(ctx context.Context, id string) (Status, error) {
	return call(ctx, e, func(s *state) (Status, error) {
		if j := s.jobs[id]; j != nil {
			return j.status(s.now), nil
		}
		return Status{}, ErrMissing
	})
}

func (e *Engine) Cancel(ctx context.Context, id string) (Status, error) {
	return call(ctx, e, func(s *state) (Status, error) {
		j := s.jobs[id]
		if j == nil {
			return Status{}, ErrMissing
		}
		if j != s.active {
			return Status{}, ErrConflict
		}
		s.finish("CANCELLED", "operator cancelled")
		return j.status(s.now), nil
	})
}

func (e *Engine) Tiles(ctx context.Context, id string, cursor int) (Page, error) {
	return call(ctx, e, func(s *state) (Page, error) {
		j := s.jobs[id]
		if j == nil {
			return Page{}, ErrMissing
		}
		if cursor < 0 || cursor > len(j.tiles) {
			return Page{}, ErrConflict
		}
		end := min(cursor+32, len(j.tiles))
		return Page{append([]workload.Tile{}, j.tiles[cursor:end]...), end}, nil
	})
}

func (e *Engine) Result(ctx context.Context, id string) (workload.Input, []workload.Tile, error) {
	type snapshot struct {
		input workload.Input
		tiles []workload.Tile
	}
	v, err := call(ctx, e, func(s *state) (snapshot, error) {
		j := s.jobs[id]
		if j == nil {
			return snapshot{}, ErrMissing
		}
		if j.State != "SUCCEEDED" {
			return snapshot{}, ErrConflict
		}
		return snapshot{j.Input, j.tiles}, nil
	})
	return v.input, v.tiles, err
}

func (e *Engine) Workers(ctx context.Context) ([]WorkerStatus, error) {
	return call(ctx, e, func(s *state) ([]WorkerStatus, error) {
		out := append([]WorkerStatus{}, s.lost...)
		for _, w := range s.workers {
			v := w.WorkerStatus
			if v.State != "paused" && v.Active > 0 {
				v.State = "active"
			}
			out = append(out, v)
		}
		return out, nil
	})
}

func (e *Engine) Join(ctx context.Context, slots int, requestID string) (Session, error) {
	if slots < 1 || slots > 4 {
		return Session{}, ErrCapacity
	}
	return call(ctx, e, func(s *state) (Session, error) {
		if len(s.workers) >= MaxWorkers {
			return Session{}, ErrCapacity
		}
		w := &worker{
			WorkerStatus: WorkerStatus{ID: s.id(), Slots: slots, State: "idle"},
			seen:         s.now, out: make(chan protocol.Out, outboundSize), done: make(chan struct{}),
		}
		s.workers = append(s.workers, w)
		s.send(w, "welcome", requestID, protocol.Welcome{
			Session: w.ID, Heartbeat: heartbeat.Milliseconds(),
			HeartbeatTimeout: heartbeatTimeout.Milliseconds(), TaskTimeout: taskTimeout.Milliseconds(),
		})
		return Session{w.ID, w.out, w.done}, nil
	})
}

// Updates are serialized per connection. Result data belongs to the engine after Update.
type Update struct {
	Type, RequestID string
	Slots           int
	Lease           protocol.Lease
	Data            []byte
}

func (e *Engine) Update(ctx context.Context, id string, u Update) error {
	_, err := call(ctx, e, func(s *state) (struct{}, error) {
		for _, w := range s.workers {
			if w.ID == id {
				return struct{}{}, s.update(w, u)
			}
		}
		return struct{}{}, ErrClosed
	})
	return err
}

func (s *state) update(w *worker, u Update) error {
	switch u.Type {
	case "heartbeat":
		w.seen = s.now
	case "ready":
		if w.State != "paused" {
			w.free = max(0, min(u.Slots, w.Slots-w.Active))
		}
	case "pause":
		w.State, w.free = "paused", 0
		s.revoke(w, "paused", true)
	case "resume":
		w.State = "idle"
	case "leave":
		s.drop(w)
	case "result", "task_error":
		l := s.live[u.Lease.LeaseID]
		disposition := "stale"
		if l != nil && l.owner == w && l.Lease == u.Lease {
			if u.Type == "task_error" || !workload.ValidTile(l.task.rect, u.Data, s.active.Input.MaxIter) {
				disposition = "invalid"
				s.retry(l, "invalid result or worker error")
			} else {
				disposition = "accepted"
				s.release(l)
				j := s.active
				j.tiles = append(j.tiles, workload.Tile{Rect: l.task.rect, Data: u.Data})
				j.Accepted++
				if j.Accepted == j.Total {
					s.finish("SUCCEEDED", "")
				}
			}
		}
		if u.Type == "result" {
			s.send(w, "result_ack", u.RequestID, protocol.Ack{LeaseID: u.Lease.LeaseID, Disposition: disposition})
		}
	}
	return nil
}

func (s *state) send(w *worker, kind, requestID string, payload any) bool {
	select {
	case <-w.done:
		return false
	default:
	}
	select {
	case w.out <- protocol.Out{V: protocol.Version, Type: kind, RequestID: requestID, Payload: payload}:
		return true
	default:
		s.drop(w)
		return false
	}
}

func (s *state) release(l *lease) {
	delete(s.live, l.LeaseID)
	l.owner.Active--
}

func (s *state) retry(l *lease, reason string) {
	s.release(l)
	if l.task.attempt >= maxAttempts {
		s.finish("FAILED", "attempt limit reached: "+reason)
		return
	}
	i, _ := strconv.Atoi(l.TaskID)
	s.active.ready = append(s.active.ready, i)
	s.active.Retries++
}

func (s *state) revoke(w *worker, reason string, notify bool) {
	for _, l := range s.live {
		if l.owner != w {
			continue
		}
		s.retry(l, reason)
		if notify {
			s.send(w, "cancel", "", protocol.Cancel{LeaseID: l.LeaseID, Reason: reason})
		}
	}
}

func (s *state) drop(w *worker) {
	for i, existing := range s.workers {
		if existing != w {
			continue
		}
		s.workers = append(s.workers[:i], s.workers[i+1:]...)
		close(w.done)
		s.revoke(w, "worker disconnected", false)
		if len(s.lost) == MaxWorkers {
			s.lost = s.lost[1:]
		}
		s.lost = append(s.lost, WorkerStatus{ID: w.ID, State: "lost", Slots: w.Slots})
		return
	}
}

func (s *state) finish(status, reason string) {
	j := s.active
	j.State, j.Reason, j.ended = status, reason, s.now
	j.ready, j.tasks = nil, nil
	s.active = nil
	// Remove all leases before notifying any connection that might back up.
	var revoked []*lease
	for _, l := range s.live {
		s.release(l)
		revoked = append(revoked, l)
	}
	for _, l := range revoked {
		s.send(l.owner, "cancel", "", protocol.Cancel{LeaseID: l.LeaseID, Reason: reason})
	}
}

func (s *state) expire() {
	if s.active != nil && !s.now.Before(s.active.deadline) {
		s.finish("FAILED", "job deadline exceeded")
	}
	for i := len(s.workers) - 1; i >= 0; i-- {
		if i >= len(s.workers) {
			continue
		}
		w := s.workers[i]
		if !s.now.Before(w.seen.Add(heartbeatTimeout)) {
			s.drop(w)
		}
	}
	for _, l := range s.live {
		if !s.now.Before(l.until) {
			s.retry(l, "lease expired")
			s.send(l.owner, "cancel", "", protocol.Cancel{LeaseID: l.LeaseID, Reason: "lease expired"})
		}
	}
	for id, j := range s.jobs {
		if !j.ended.IsZero() && !s.now.Before(j.ended.Add(retention)) {
			delete(s.jobs, id)
			s.bytes -= j.bytes
		}
	}
}

func (s *state) dispatch() {
	// Snapshot because a full outbound queue can evict a worker.
	for _, w := range append([]*worker(nil), s.workers...) {
		select {
		case <-w.done:
			continue
		default:
		}
		for w.free > 0 && w.Active < w.Slots && s.active != nil && len(s.active.ready) > 0 {
			j := s.active
			i := j.ready[0]
			j.ready = j.ready[1:]
			t := &j.tasks[i]
			t.attempt++
			window := min(taskTimeout, j.deadline.Sub(s.now))
			l := &lease{
				Lease: protocol.Lease{JobID: j.ID, TaskID: strconv.Itoa(i), LeaseID: s.id()},
				owner: w, task: t, until: s.now.Add(window),
			}
			s.live[l.LeaseID] = l
			j.State = "RUNNING"
			w.Active++
			w.free--
			if !s.send(w, "assign", "", protocol.Assignment{
				Lease: l.Lease, Attempt: t.attempt, Kernel: workload.Kernel, KernelVersion: 1,
				Params: workload.Params{Input: j.Input, Rect: t.rect}, Deadline: window.Milliseconds(),
			}) {
				break
			}
		}
	}
}

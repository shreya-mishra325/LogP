package engine

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"browsercompute.local/runtime/internal/protocol"
	"browsercompute.local/runtime/internal/workload"
)

type fixture struct {
	t   *testing.T
	ctx context.Context
	e   *Engine
	now atomic.Int64
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	f := &fixture{t: t, ctx: ctx}
	n := 0
	f.e = New(ctx, func() time.Time { return time.Unix(100, f.now.Load()) }, func() string { n++; return strconv.Itoa(n) })
	t.Cleanup(func() { stop(); <-f.e.Done() })
	return f
}

func (f *fixture) send(s Session, u Update) {
	f.t.Helper()
	if err := f.e.Update(f.ctx, s.ID, u); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) take(s Session, kind string) any {
	f.t.Helper()
	select {
	case m := <-s.Out:
		if m.Type != kind {
			f.t.Fatalf("want %s, got %s", kind, m.Type)
		}
		return m.Payload
	case <-f.ctx.Done():
		f.t.Fatalf("missing %s", kind)
	}
	return nil
}

func (f *fixture) join() Session {
	f.t.Helper()
	s, err := f.e.Join(f.ctx, 1, "")
	if err != nil {
		f.t.Fatal(err)
	}
	f.take(s, "welcome")
	return s
}

func (f *fixture) assign(s Session) protocol.Assignment {
	f.t.Helper()
	f.send(s, Update{Type: "ready", Slots: 4})
	return f.take(s, "assign").(protocol.Assignment)
}

func input() workload.Input {
	return workload.Input{Kernel: workload.Kernel, Width: 1, Height: 1, MaxIter: 32, TileSize: 16, XMin: -2, XMax: 1, YMin: -1, YMax: 1}
}

func (f *fixture) submit() Status {
	f.t.Helper()
	j, err := f.e.Submit(f.ctx, input())
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

func (f *fixture) result(s Session, l protocol.Lease, data []byte, want string) {
	f.t.Helper()
	f.send(s, Update{Type: "result", Lease: l, Data: data})
	if got := f.take(s, "result_ack").(protocol.Ack).Disposition; got != want {
		f.t.Fatalf("got %s want %s", got, want)
	}
}

func TestLeaseRecoveryAndAtomicAcceptance(t *testing.T) {
	f := setup(t)
	a, b := f.join(), f.join()
	j := f.submit()
	l := f.assign(a)
	f.result(b, l.Lease, []byte{0, 0}, "stale")
	wrong := l.Lease
	wrong.TaskID = "wrong"
	f.result(a, wrong, []byte{0, 0}, "stale")
	f.send(a, Update{Type: "pause"})
	f.take(a, "cancel")
	f.send(a, Update{Type: "ready", Slots: 1})
	retry := f.assign(b)
	if retry.Attempt != 2 || retry.LeaseID == l.LeaseID {
		t.Fatalf("incorrect retry %+v", retry)
	}
	f.result(a, l.Lease, []byte{1, 0}, "stale")
	f.result(b, retry.Lease, []byte{2, 0}, "accepted")
	f.result(b, retry.Lease, []byte{3, 0}, "stale")
	_, tiles, err := f.e.Result(f.ctx, j.ID)
	if err != nil || len(tiles) != 1 || tiles[0].Data[0] != 2 {
		t.Fatalf("accepted data changed: %v %v", tiles, err)
	}
}

func TestExactExpiryAndRetryExhaustion(t *testing.T) {
	f := setup(t)
	w := f.join()
	j := f.submit()
	l := f.assign(w)
	f.now.Store(int64(7 * time.Second))
	f.send(w, Update{Type: "heartbeat"})
	f.now.Store(int64(10 * time.Second))
	f.send(w, Update{Type: "result", Lease: l.Lease, Data: []byte{1, 0}})
	f.take(w, "cancel")
	if got := f.take(w, "result_ack").(protocol.Ack).Disposition; got != "stale" {
		t.Fatal(got)
	}
	l = f.assign(w)
	f.result(w, l.Lease, []byte{33, 0}, "invalid")
	l = f.assign(w)
	f.result(w, l.Lease, nil, "invalid")
	v, err := f.e.Status(f.ctx, j.ID)
	if err != nil || v.State != "FAILED" || v.Accepted != 0 || v.Retries != 2 {
		t.Fatalf("%+v %v", v, err)
	}
	if _, _, err = f.e.Result(f.ctx, j.ID); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

func TestDisconnectCancelAndDeadline(t *testing.T) {
	f := setup(t)
	w := f.join()
	j := f.submit()
	f.assign(w)
	f.send(w, Update{Type: "leave"})
	select {
	case <-w.Done:
	default:
		t.Fatal("session still open")
	}
	b := f.join()
	l := f.assign(b)
	if l.Attempt != 2 {
		t.Fatal(l)
	}
	if _, err := f.e.Cancel(f.ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	f.take(b, "cancel")
	f.result(b, l.Lease, []byte{0, 0}, "stale")
	j = f.submit()
	if v, _ := f.e.Status(f.ctx, j.ID); v.State != "QUEUED" {
		t.Fatal(v)
	}
	f.now.Store(int64(jobTimeout))
	if v, _ := f.e.Status(f.ctx, j.ID); v.State != "FAILED" {
		t.Fatal(v)
	}
	f.now.Add(int64(retention))
	if _, err := f.e.Status(f.ctx, j.ID); !errors.Is(err, ErrMissing) {
		t.Fatal(err)
	}
}

func TestCapsAndBackpressure(t *testing.T) {
	f := setup(t)
	w := f.join()
	for range MaxWorkers - 1 {
		f.join()
	}
	if _, err := f.e.Join(f.ctx, 1, ""); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	l := protocol.Lease{JobID: "x", TaskID: "0", LeaseID: "y"}
	for range outboundSize + 1 {
		f.send(w, Update{Type: "result", Lease: l})
	}
	select {
	case <-w.Done:
	default:
		t.Fatal("slow worker not dropped")
	}
	large := input()
	large.Width, large.Height = 2048, 2048
	for range maxBytes / (2 * 2048 * 2048) {
		j, err := f.e.Submit(f.ctx, large)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.e.Submit(f.ctx, input()); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
		if _, err := f.e.Cancel(f.ctx, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.e.Submit(f.ctx, input()); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
}

func TestHeartbeatTimeoutAndJobCount(t *testing.T) {
	f := setup(t)
	worker := f.join()
	j := f.submit()
	l := f.assign(worker)
	f.now.Store(int64(heartbeatTimeout))
	workers, err := f.e.Workers(f.ctx)
	if err != nil || len(workers) != 1 || workers[0].State != "lost" {
		t.Fatalf("session survived heartbeat deadline: %+v %v", workers, err)
	}
	next := f.join()
	retry := f.assign(next)
	if retry.TaskID != l.TaskID || retry.Attempt != 2 {
		t.Fatal(retry)
	}
	if _, err := f.e.Cancel(f.ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	for range maxJobs - 1 {
		j := f.submit()
		if _, err := f.e.Cancel(f.ctx, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.e.Submit(f.ctx, input()); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
}

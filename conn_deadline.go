package webdial

import (
	"sync"
	"time"
)

// connDeadline stores an absolute deadline and notifies operations whenever it
// changes. Operations recalculate their timer instead of being canceled on
// every update, so extending or clearing a deadline also affects blocked I/O.
type connDeadline struct {
	mu      sync.Mutex
	when    time.Time
	changed chan struct{}
}

func newConnDeadline() connDeadline {
	return connDeadline{changed: make(chan struct{})}
}

func (d *connDeadline) set(when time.Time) {
	d.mu.Lock()
	oldChanged := d.changed
	d.when = when
	d.changed = make(chan struct{})
	close(oldChanged)
	d.mu.Unlock()
}

type deadlineSnapshot struct {
	timer   *time.Timer
	expired bool
	changed <-chan struct{}
}

func (d *connDeadline) snapshot() deadlineSnapshot {
	d.mu.Lock()
	when := d.when
	changed := d.changed
	d.mu.Unlock()

	snapshot := deadlineSnapshot{changed: changed}
	if when.IsZero() {
		return snapshot
	}
	delay := time.Until(when)
	if delay <= 0 {
		snapshot.expired = true
		return snapshot
	}
	snapshot.timer = time.NewTimer(delay)
	return snapshot
}

func (d *connDeadline) expired() bool {
	d.mu.Lock()
	when := d.when
	d.mu.Unlock()
	return !when.IsZero() && !time.Now().Before(when)
}

func (s deadlineSnapshot) timerC() <-chan time.Time {
	if s.timer == nil {
		return nil
	}
	return s.timer.C
}

func (s deadlineSnapshot) stop() {
	if s.timer != nil {
		s.timer.Stop()
	}
}

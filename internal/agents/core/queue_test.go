package core

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInvocationQueueRunsJobsOneAtATimeInOrder(t *testing.T) {
	var (
		mu      sync.Mutex
		order   []int
		active  int32
		maxSeen int32
		busy    []bool
	)
	q := NewInvocationQueue(func(b bool) {
		mu.Lock()
		busy = append(busy, b)
		mu.Unlock()
	})
	q.Reopen()

	for i := 0; i < 4; i++ {
		i := i
		if !q.Enqueue(func() {
			n := atomic.AddInt32(&active, 1)
			if n > maxSeen {
				atomic.StoreInt32(&maxSeen, n)
			}
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			atomic.AddInt32(&active, -1)
		}) {
			t.Fatal("enqueue refused")
		}
	}
	q.Wait()

	if maxSeen != 1 {
		t.Errorf("saw %d jobs at once, want 1", maxSeen)
	}
	for i, v := range order {
		if v != i {
			t.Fatalf("order = %v, want FIFO", order)
		}
	}
	if len(busy) != 2 || !busy[0] || busy[1] {
		t.Errorf("busy notifications = %v, want [true false]", busy)
	}
}

func TestInvocationQueueCloseDropsPendingAndRefuses(t *testing.T) {
	q := NewInvocationQueue(nil)
	if q.Enqueue(func() {}) {
		t.Fatal("a never-opened queue must refuse work")
	}
	q.Reopen()

	release := make(chan struct{})
	var ran int32
	started := make(chan struct{})
	q.Enqueue(func() { close(started); <-release; atomic.AddInt32(&ran, 1) })
	q.Enqueue(func() { atomic.AddInt32(&ran, 1) })
	q.Enqueue(func() { atomic.AddInt32(&ran, 1) })

	<-started
	q.Close()
	if q.Enqueue(func() {}) {
		t.Error("closed queue accepted a job")
	}
	close(release)
	q.Wait()
	if ran != 1 {
		t.Errorf("ran %d jobs, want only the one already running", ran)
	}
}

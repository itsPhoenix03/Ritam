package core

import "sync"

// InvocationQueue runs an adapter's CLI invocations one at a time, in the
// order they were submitted.
//
// Without it, every message that reached a busy agent started its own CLI
// process alongside the one already running. The authority once received two
// PROPOSALs eight seconds apart and ran two `claude` processes in parallel:
// each reasoned from the state it had been handed, neither saw the other's
// conclusion, and the second answer was stale before it was written. Queueing
// makes the second message wait, so it is answered with the first one done.
//
// Enqueue never blocks on a running job, which keeps the orchestrator's run
// loop responsive: jobs run on the queue's own worker goroutine.
type InvocationQueue struct {
	mu      sync.Mutex
	pending []func()
	running bool
	closed  bool
	wg      sync.WaitGroup
	// onBusy is told true when work appears on an idle queue and false when
	// the queue drains. It is called with mu held so a drain and a fresh
	// enqueue can never deliver their notifications out of order, which would
	// leave an agent reported IDLE while a call is running. It must therefore
	// not call back into the queue.
	onBusy func(busy bool)
}

// NewInvocationQueue builds a queue. onBusy may be nil.
func NewInvocationQueue(onBusy func(busy bool)) *InvocationQueue {
	return &InvocationQueue{onBusy: onBusy, closed: true}
}

// Reopen allows submissions again. A queue starts closed, matching adapters,
// which refuse to send until started.
func (q *InvocationQueue) Reopen() {
	q.mu.Lock()
	q.closed = false
	q.mu.Unlock()
}

// Enqueue schedules job behind any earlier ones. It reports false when the
// queue is closed and the job was not accepted.
func (q *InvocationQueue) Enqueue(job func()) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	q.pending = append(q.pending, job)
	if !q.running {
		q.running = true
		q.wg.Add(1)
		if q.onBusy != nil {
			q.onBusy(true)
		}
		go q.work()
	}
	return true
}

// Close refuses new jobs and discards the ones not yet started. A job already
// running is not interrupted here: the caller cancels its context, then Wait.
func (q *InvocationQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.pending = nil
	q.mu.Unlock()
}

// Wait blocks until the running job, if any, has returned.
func (q *InvocationQueue) Wait() { q.wg.Wait() }

// Len reports how many jobs are running or waiting.
func (q *InvocationQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.pending)
	if q.running {
		n++
	}
	return n
}

func (q *InvocationQueue) work() {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		if len(q.pending) == 0 {
			q.running = false
			if q.onBusy != nil {
				q.onBusy(false)
			}
			q.mu.Unlock()
			return
		}
		job := q.pending[0]
		q.pending = q.pending[1:]
		q.mu.Unlock()
		job()
	}
}

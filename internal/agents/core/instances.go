package core

import (
	"context"
	"fmt"
	"sync"
	"time"

	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
)

// Per-task agent instances.
//
// An agent is a definition -- a CLI, a model, a role -- and each task it
// works on gets its own instance of it: its own adapter, CLI processes and
// sessions. The same agent can therefore work on several tasks at once
// without one task's run stopping the other's processes or reading its
// replies. An agent registered with a single shared adapter (tests, simple
// embeddings) keeps the old behaviour: one adapter for every task.
//
// Each running instance is watched, and a change of state (IDLE, WORKING,
// error) is reported through OnInstanceChange, so the screen can show an
// agent working the moment it is.

// instancePoll is how often a running instance's state is checked.
var instancePoll = 250 * time.Millisecond

type instKey struct {
	task domain.TaskID
	id   domain.AgentID
}

type instance struct {
	ad     ports.AgentAdapter
	owned  bool // made by the agent's factory, so stopped and discarded with the task
	cancel context.CancelFunc
	state  domain.AgentState
}

type instances struct {
	mu  sync.Mutex
	set map[instKey]*instance
}

func (m *Manager) inst() *instances {
	m.instOnce.Do(func() { m.instSet = &instances{set: map[instKey]*instance{}} })
	return m.instSet
}

// StartFor starts agent id for task: a fresh instance when the agent has a
// factory, otherwise its shared adapter.
func (m *Manager) StartFor(ctx context.Context, task domain.TaskID, id domain.AgentID) error {
	var ad ports.AgentAdapter
	owned := false
	if f, ok := m.reg.Factory(id); ok {
		made, err := f()
		if err != nil {
			m.setState(id, domain.AgentStateError, err.Error())
			return fmt.Errorf("core: create agent %q: %w", id, err)
		}
		if err := made.Start(ctx); err != nil {
			m.setState(id, domain.AgentStateError, err.Error())
			return fmt.Errorf("core: start agent %q: %w", id, err)
		}
		ad, owned = made, true
	} else {
		if err := m.Start(ctx, id); err != nil {
			return err
		}
		ad, _ = m.reg.Adapter(id)
	}

	watchCtx, cancel := context.WithCancel(context.Background())
	in := &instance{ad: ad, owned: owned, cancel: cancel, state: ad.Status()}
	set := m.inst()
	set.mu.Lock()
	if old, ok := set.set[instKey{task, id}]; ok {
		old.cancel()
	}
	set.set[instKey{task, id}] = in
	set.mu.Unlock()
	go m.watch(watchCtx, task, id, in)
	m.refresh(id, "")
	m.notify(task, id, in.state)
	return nil
}

// StopFor stops agent id's instance for task.
func (m *Manager) StopFor(ctx context.Context, task domain.TaskID, id domain.AgentID) error {
	set := m.inst()
	set.mu.Lock()
	in, ok := set.set[instKey{task, id}]
	delete(set.set, instKey{task, id})
	set.mu.Unlock()
	if !ok {
		if _, has := m.reg.Factory(id); has {
			return nil
		}
		return m.Stop(ctx, id)
	}
	in.cancel()
	var err error
	if in.owned {
		err = in.ad.Stop(ctx)
		m.refresh(id, "")
	} else {
		err = m.Stop(ctx, id)
	}
	m.notify(task, id, domain.AgentStateStopped)
	return err
}

// AdapterFor returns the adapter agent id uses on task.
func (m *Manager) AdapterFor(task domain.TaskID, id domain.AgentID) (ports.AgentAdapter, bool) {
	set := m.inst()
	set.mu.Lock()
	in, ok := set.set[instKey{task, id}]
	set.mu.Unlock()
	if ok {
		return in.ad, true
	}
	return m.reg.Adapter(id)
}

// ReportOutboundFor records a failure reported by agent id's instance on task.
func (m *Manager) ReportOutboundFor(task domain.TaskID, id domain.AgentID, out ports.Outbound) {
	if out.Err == nil {
		return
	}
	msg := logging.Redact(out.Err.Error())
	state := domain.AgentStateError
	if ad, ok := m.AdapterFor(task, id); ok && ad.Status() == domain.AgentStateUnavailable {
		state = domain.AgentStateUnavailable
	}
	m.setState(id, state, msg)
	m.notify(task, id, state)
	m.log.ForAgent(id).Error("agent runtime reported failure", "taskId", string(task), "error", msg)
}

// TaskStatuses returns every agent as it stands on task: the state of its
// instance there, stopped when it has none.
func (m *Manager) TaskStatuses(task domain.TaskID) []domain.Agent {
	out := m.reg.List()
	set := m.inst()
	set.mu.Lock()
	defer set.mu.Unlock()
	for i := range out {
		if in, ok := set.set[instKey{task, out[i].ID}]; ok {
			out[i].State = in.ad.Status()
		} else if out[i].State != domain.AgentStateError && out[i].State != domain.AgentStateUnavailable {
			out[i].State = domain.AgentStateStopped
		}
	}
	return out
}

// watch reports the instance's state changes until it is stopped.
func (m *Manager) watch(ctx context.Context, task domain.TaskID, id domain.AgentID, in *instance) {
	tick := time.NewTicker(instancePoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s := in.ad.Status()
			m.inst().mu.Lock()
			changed := s != in.state
			in.state = s
			m.inst().mu.Unlock()
			if changed {
				m.refresh(id, "")
				m.notify(task, id, s)
			}
		}
	}
}

// refresh sets the agent's overall state from its running instances: WORKING
// if any instance is, IDLE if any is running, STOPPED otherwise. An error
// state stands until an instance starts again.
func (m *Manager) refresh(id domain.AgentID, errMsg string) {
	set := m.inst()
	set.mu.Lock()
	running, working := false, false
	for k, in := range set.set {
		if k.id != id {
			continue
		}
		switch in.ad.Status() {
		case domain.AgentStateWorking:
			working, running = true, true
		case domain.AgentStateIdle, domain.AgentStateStarting:
			running = true
		}
	}
	set.mu.Unlock()
	state := domain.AgentStateStopped
	switch {
	case working:
		state = domain.AgentStateWorking
	case running:
		state = domain.AgentStateIdle
	}
	if cur, ok := m.reg.Get(id); ok && !running &&
		(cur.State == domain.AgentStateError || cur.State == domain.AgentStateUnavailable) {
		return
	}
	m.reg.SetState(id, state, errMsg)
}

func (m *Manager) notify(task domain.TaskID, id domain.AgentID, state domain.AgentState) {
	if m.OnInstanceChange == nil {
		return
	}
	if a, ok := m.reg.Get(id); ok {
		a.State = state
		m.OnInstanceChange(task, a)
	}
}

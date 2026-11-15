package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"ritam/internal/domain"
	"ritam/internal/ports"
	"ritam/internal/storage"
)

// taskBackends runs a test against both stores: the lost update was a
// storage-level race as much as a TaskManager one.
func taskBackends(t *testing.T, run func(t *testing.T, s *ports.Storage)) {
	t.Run("memory", func(t *testing.T) { run(t, storage.NewInMemory()) })
	t.Run("sqlite", func(t *testing.T) {
		db, err := storage.OpenSQLite(filepath.Join(t.TempDir(), "ritam.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		run(t, db.Storage())
	})
}

func newChangeTask(t *testing.T, tm *TaskManager) *domain.Task {
	t.Helper()
	task, err := tm.Create(context.Background(), ports.CreateTaskInput{
		Title: "race", Authority: "opus", Participants: []domain.AgentID{"opus", "codex"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// The run loop records messages while the human, on the UI goroutine,
// changes the authority and speaks. Each used to write back a whole copy of
// the task read before the other's change, so the authority change was
// reverted and messages went uncounted.
func TestTaskManager_AnAuthorityChangeSurvivesAThousandMessagesRecordedAtTheSameTime(t *testing.T) {
	taskBackends(t, func(t *testing.T, s *ports.Storage) {
		ctx := context.Background()
		tm := NewTaskManager(s.Tasks, NewEventBus(s.Events, nil), domain.TaskLimits{}, nil)
		task := newChangeTask(t, tm)

		const messages = 1000
		var recorded atomic.Int64
		var stopped atomic.Bool
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer stopped.Store(true)
			for i := 0; i < messages; i++ {
				m := &domain.Message{FromAgent: "codex", ToAgent: "opus", Type: domain.MsgInformation}
				if _, err := tm.NoteMessage(ctx, task.ID, m, Neutral); err != nil {
					errs <- err
					return
				}
				recorded.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			// Keep writing while messages are recorded, so a message's stale
			// copy has every chance to land after one of these writes, and
			// make the last authority change half-way through.
			for i := 0; recorded.Load() < messages/2 && !stopped.Load(); i++ {
				to := domain.AgentID("opus")
				if i%2 == 1 {
					to = "codex"
				}
				if _, err := tm.SetAuthority(ctx, task.ID, to); err != nil {
					errs <- err
					return
				}
				if _, err := tm.NoteHumanInput(ctx, task.ID); err != nil {
					errs <- err
					return
				}
			}
			if _, err := tm.SetAuthority(ctx, task.ID, "codex"); err != nil {
				errs <- err
				return
			}
			for i := 0; i < 50; i++ {
				if _, err := tm.NoteHumanInput(ctx, task.ID); err != nil {
					errs <- err
					return
				}
			}
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}

		got, err := tm.Get(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.AuthorityAgent != "codex" {
			t.Errorf("authority = %s: the change was reverted by a concurrent write", got.AuthorityAgent)
		}
		if got.MessageCount != messages {
			t.Errorf("message count = %d, want %d", got.MessageCount, messages)
		}
	})
}

// sneakyTasks is a task repository on which another writer changes the
// task between this TaskManager's read and its write, as many times as
// asked.
type sneakyTasks struct {
	ports.TaskRepository
	sneaks  int
	updates int
}

func (r *sneakyTasks) Update(ctx context.Context, t *domain.Task) error {
	r.updates++
	if r.sneaks > 0 {
		r.sneaks--
		other, err := r.TaskRepository.Get(ctx, t.ID)
		if err != nil {
			return err
		}
		other.Description += "+"
		if err := r.TaskRepository.Update(ctx, other); err != nil {
			return err
		}
	}
	return r.TaskRepository.Update(ctx, t)
}

func TestTaskManager_RedoesAChangeThatAnotherWriterBeatItTo(t *testing.T) {
	taskBackends(t, func(t *testing.T, s *ports.Storage) {
		ctx := context.Background()
		repo := &sneakyTasks{TaskRepository: s.Tasks}
		tm := NewTaskManager(repo, NewEventBus(s.Events, nil), domain.TaskLimits{}, nil)
		task := newChangeTask(t, tm)

		repo.sneaks = 2
		if _, err := tm.SetAuthority(ctx, task.ID, "codex"); err != nil {
			t.Fatal(err)
		}
		got, _ := tm.Get(ctx, task.ID)
		if got.AuthorityAgent != "codex" || got.Description != "++" {
			t.Fatalf("both writers' changes must be kept: authority %s, description %q",
				got.AuthorityAgent, got.Description)
		}
	})
}

func TestTaskManager_GivesUpOnATaskThatNeverStopsChanging(t *testing.T) {
	s := storage.NewInMemory()
	ctx := context.Background()
	repo := &sneakyTasks{TaskRepository: s.Tasks}
	tm := NewTaskManager(repo, NewEventBus(s.Events, nil), domain.TaskLimits{}, nil)
	task := newChangeTask(t, tm)

	repo.sneaks, repo.updates = 1000, 0
	_, err := tm.SetAuthority(ctx, task.ID, "codex")
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("got %v, want ErrConflict once the attempts run out", err)
	}
	if repo.updates != maxChangeAttempts {
		t.Fatalf("tried %d times, want %d", repo.updates, maxChangeAttempts)
	}
}

func TestTaskManager_ResumeIsOneWrite(t *testing.T) {
	taskBackends(t, func(t *testing.T, s *ports.Storage) {
		ctx := context.Background()
		repo := &sneakyTasks{TaskRepository: s.Tasks}
		bus := NewEventBus(s.Events, nil)
		tm := NewTaskManager(repo, bus, domain.TaskLimits{}, nil)
		task := newChangeTask(t, tm)
		for _, to := range []domain.TaskState{domain.TaskPlanning, domain.TaskImplementing} {
			if _, err := tm.Transition(ctx, task.ID, to, "opus", ""); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tm.Pause(ctx, task.ID, domain.HumanAgentID, "limit"); err != nil {
			t.Fatal(err)
		}
		var seen []domain.EventType
		unsubscribe := bus.Subscribe(func(e domain.Event) { seen = append(seen, e.Type) })
		defer unsubscribe()

		repo.updates = 0
		got, err := tm.Resume(ctx, task.ID, domain.HumanAgentID)
		if err != nil {
			t.Fatal(err)
		}
		if repo.updates != 1 {
			t.Errorf("Resume wrote the task %d times, want once", repo.updates)
		}
		if got.State != domain.TaskImplementing || got.AgentMessages != 0 {
			t.Errorf("resumed task: %+v", got)
		}
		want := []domain.EventType{domain.EvtTaskStateChanged, domain.EvtTaskResumed}
		if len(seen) != len(want) || seen[0] != want[0] || seen[1] != want[1] {
			t.Errorf("events = %v, want %v", seen, want)
		}
	})
}

func TestTaskManager_AResumeThatCannotHappenChangesNothing(t *testing.T) {
	s := storage.NewInMemory()
	ctx := context.Background()
	tm := NewTaskManager(s.Tasks, NewEventBus(s.Events, nil), domain.TaskLimits{}, nil)
	task := newChangeTask(t, tm)
	before, _ := tm.Get(ctx, task.ID)
	if _, err := tm.Resume(ctx, task.ID, domain.HumanAgentID); err == nil {
		t.Fatal("resuming a task that is not paused must fail")
	}
	after, _ := tm.Get(ctx, task.ID)
	if after.Version != before.Version {
		t.Fatalf("a refused resume wrote the task (version %d -> %d)", before.Version, after.Version)
	}
}

func TestTaskManager_ATransitionToTheCurrentStateWritesNothing(t *testing.T) {
	s := storage.NewInMemory()
	ctx := context.Background()
	repo := &sneakyTasks{TaskRepository: s.Tasks}
	tm := NewTaskManager(repo, NewEventBus(s.Events, nil), domain.TaskLimits{}, nil)
	task := newChangeTask(t, tm)
	repo.updates = 0
	got, err := tm.Transition(ctx, task.ID, domain.TaskCreated, "opus", "")
	if err != nil || got.State != domain.TaskCreated {
		t.Fatalf("%v %+v", err, got)
	}
	if repo.updates != 0 {
		t.Fatalf("a no-op transition wrote the task %d times", repo.updates)
	}
}

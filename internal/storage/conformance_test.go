package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// backends runs a test against every storage implementation, so the SQLite
// store is held to exactly the in-memory store's semantics.
func backends(t *testing.T, run func(t *testing.T, s *ports.Storage)) {
	t.Run("memory", func(t *testing.T) { run(t, NewInMemory()) })
	t.Run("sqlite", func(t *testing.T) {
		db, err := OpenSQLite(filepath.Join(t.TempDir(), "ritam.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		run(t, db.Storage())
	})
}

func TestConformance_Tasks(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		now := time.Now()
		task := &domain.Task{
			ID: "TASK-001", Title: "first", Participants: []domain.AgentID{"a", "b"},
			State: domain.TaskDeliberating, Limits: domain.TaskLimits{MaxDuration: 30 * time.Minute, MaxCostUSD: 2.5},
			CostUSD: 0.42, InputTokens: 1234, FollowUp: true, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.Tasks.Create(c, task); err != nil {
			t.Fatal(err)
		}
		if err := s.Tasks.Create(c, task); !errors.Is(err, ports.ErrAlreadyExists) {
			t.Fatalf("duplicate: %v", err)
		}
		task.Participants[0] = "mutated"
		got, err := s.Tasks.Get(c, "TASK-001")
		if err != nil {
			t.Fatal(err)
		}
		if got.Participants[0] != "a" || got.Limits.MaxDuration != 30*time.Minute || got.CostUSD != 0.42 ||
			got.InputTokens != 1234 || !got.FollowUp || !got.CreatedAt.Equal(now) {
			t.Fatalf("round trip lost data: %+v", got)
		}
		// The session registry keys sessions by the task's creation time, so it
		// must come back to the nanosecond.
		if got.CreatedAt.UTC().Format(time.RFC3339Nano) != now.UTC().Format(time.RFC3339Nano) {
			t.Fatalf("created_at drifted: %v vs %v", got.CreatedAt, now)
		}
		if _, err := s.Tasks.Get(c, "nope"); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
		if err := s.Tasks.Update(c, &domain.Task{ID: "TASK-999"}); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("update missing: %v", err)
		}
		done := time.Now()
		got.State, got.CompletedAt = domain.TaskCompleted, &done
		if err := s.Tasks.Update(c, got); err != nil {
			t.Fatal(err)
		}
		if again, _ := s.Tasks.Get(c, "TASK-001"); again.State != domain.TaskCompleted || again.CompletedAt == nil {
			t.Fatalf("update not persisted: %+v", again)
		}

		for _, id := range []domain.TaskID{"TASK-003", "TASK-002"} {
			if err := s.Tasks.Create(c, &domain.Task{ID: id}); err != nil {
				t.Fatal(err)
			}
		}
		list, _ := s.Tasks.List(c)
		if len(list) != 3 || list[0].ID != "TASK-001" || list[1].ID != "TASK-003" || list[2].ID != "TASK-002" {
			t.Fatalf("list must be in creation order: %v", ids(list))
		}
		for want := 1; want <= 3; want++ {
			if n, _ := s.Tasks.NextSequence(c); n != want {
				t.Fatalf("NextSequence = %d, want %d", n, want)
			}
		}
	})
}

func ids(ts []*domain.Task) []domain.TaskID {
	out := make([]domain.TaskID, len(ts))
	for i, t := range ts {
		out[i] = t.ID
	}
	return out
}

func TestConformance_Messages(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		ts := time.Now()
		for i, id := range []domain.MessageID{"M3", "M1", "M2"} {
			m := &domain.Message{ID: id, TaskID: "T", FromAgent: "opus", ToAgent: "codex", Type: domain.MsgQuestion,
				Content: "hello\nworld", Timestamp: ts.Add(time.Duration(i)), RequiresResponse: i == 0,
				InReplyTo: "M0", Round: i}
			if i == 1 {
				m.Metadata = map[string]string{"verdict": "approve"}
			}
			if err := s.Messages.Append(c, m); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Messages.Append(c, &domain.Message{ID: "M1", TaskID: "T"}); !errors.Is(err, ports.ErrAlreadyExists) {
			t.Fatalf("duplicate: %v", err)
		}
		_ = s.Messages.Append(c, &domain.Message{ID: "OTHER", TaskID: "U"})

		list, _ := s.Messages.ListByTask(c, "T")
		if len(list) != 3 || list[0].ID != "M3" || list[1].ID != "M1" || list[2].ID != "M2" {
			t.Fatalf("messages must keep append order")
		}
		m := list[0]
		if m.Content != "hello\nworld" || !m.RequiresResponse || m.InReplyTo != "M0" || m.Type != domain.MsgQuestion ||
			!m.Timestamp.Equal(ts) || m.Metadata != nil {
			t.Fatalf("round trip: %+v", m)
		}
		if list[1].Metadata["verdict"] != "approve" || list[1].Round != 1 {
			t.Fatalf("metadata/round lost: %+v", list[1])
		}
		if n, _ := s.Messages.CountByTask(c, "T"); n != 3 {
			t.Fatalf("count = %d", n)
		}
		if got, err := s.Messages.Get(c, "M2"); err != nil || got.Round != 2 {
			t.Fatalf("get: %v %+v", err, got)
		}
		if _, err := s.Messages.Get(c, "nope"); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
	})
}

func TestConformance_Events(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		_ = s.Events.Append(c, domain.Event{ID: "E1", Type: domain.EvtTaskCreated, TaskID: "T", Summary: "a",
			Details: map[string]string{"k": "v"}, Timestamp: time.Now()})
		_ = s.Events.Append(c, domain.Event{ID: "E2", Type: domain.EvtAgentStateChanged, AgentID: "opus"})
		_ = s.Events.Append(c, domain.Event{ID: "E3", Type: domain.EvtTaskPaused, TaskID: "T"})
		task, _ := s.Events.ListByTask(c, "T")
		if len(task) != 2 || task[0].ID != "E1" || task[0].Details["k"] != "v" || task[1].ID != "E3" {
			t.Fatalf("by task: %+v", task)
		}
		all, _ := s.Events.ListAll(c)
		if len(all) != 3 || all[1].AgentID != "opus" {
			t.Fatalf("all: %+v", all)
		}
	})
}

func TestConformance_DecisionsDisagreementsArtifacts(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		d := &domain.Decision{ID: "D-001", TaskID: "T", Decision: "ship", Evidence: []string{"tests"}, Status: "PROPOSED"}
		if err := s.Decisions.Create(c, d); err != nil {
			t.Fatal(err)
		}
		if err := s.Decisions.Create(c, d); !errors.Is(err, ports.ErrAlreadyExists) {
			t.Fatalf("duplicate: %v", err)
		}
		d.Status = "ACCEPTED"
		_ = s.Decisions.Update(c, d)
		got, _ := s.Decisions.Get(c, "D-001")
		if got.Status != "ACCEPTED" || got.Evidence[0] != "tests" {
			t.Fatalf("decision: %+v", got)
		}
		if n, _ := s.Decisions.NextSequence(c); n != 1 {
			t.Fatalf("decision sequence starts at 1, got %d", n)
		}
		if n, _ := s.Tasks.NextSequence(c); n != 1 {
			t.Fatalf("task and decision sequences are independent, got %d", n)
		}

		for _, id := range []domain.DisagreementID{"DIS-1", "DIS-2"} {
			_ = s.Disagreements.Create(c, &domain.Disagreement{ID: id, TaskID: "T", Claim: "x"})
		}
		dis, _ := s.Disagreements.Get(c, "DIS-1")
		dis.Resolved, dis.Resolution = true, "evidence"
		_ = s.Disagreements.Update(c, dis)
		open, _ := s.Disagreements.ListOpen(c, "T")
		all, _ := s.Disagreements.ListByTask(c, "T")
		if len(open) != 1 || open[0].ID != "DIS-2" || len(all) != 2 {
			t.Fatalf("disagreements: open=%d all=%d", len(open), len(all))
		}

		_ = s.Artifacts.Create(c, &domain.Artifact{ID: "ART-2", TaskID: "T", Location: "b.go"})
		_ = s.Artifacts.Create(c, &domain.Artifact{ID: "ART-1", TaskID: "T", Location: "a.go"})
		arts, _ := s.Artifacts.ListByTask(c, "T")
		if len(arts) != 2 || arts[0].ID != "ART-2" {
			t.Fatalf("artifacts: %+v", arts)
		}
		if _, err := s.Artifacts.Get(c, "ART-9"); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("missing artifact: %v", err)
		}
	})
}

func TestConformance_KnowledgeSearchRanksIdentically(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		base := time.Now()
		entries := []*domain.KnowledgeEntry{
			{ID: "K1", Category: domain.KnowledgeCategory("finding"), TaskID: "T", AgentID: "opus",
				Title: "auth bug", Body: "token expiry", Tags: []string{"Auth"}, CreatedAt: base},
			{ID: "K2", Category: domain.KnowledgeCategory("finding"), TaskID: "T", AgentID: "codex",
				Title: "cache", Body: "auth auth auth", CreatedAt: base.Add(time.Second)},
			{ID: "K3", Category: domain.KnowledgeCategory("decision"), TaskID: "U", AgentID: "opus",
				Title: "unrelated", Body: "nothing", CreatedAt: base.Add(2 * time.Second)},
		}
		for _, e := range entries {
			if err := s.Knowledge.Put(c, e); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Knowledge.Put(c, entries[0]); !errors.Is(err, ports.ErrAlreadyExists) {
			t.Fatalf("duplicate: %v", err)
		}
		hits, _ := s.Knowledge.Search(c, domain.KnowledgeQuery{Text: "AUTH"})
		if len(hits) != 2 || hits[0].Entry.ID != "K1" || hits[1].Entry.ID != "K2" {
			t.Fatalf("ranking: %v", hitIDs(hits))
		}
		hits, _ = s.Knowledge.Search(c, domain.KnowledgeQuery{TaskID: "T", Tags: []string{"auth"}})
		if len(hits) != 1 || hits[0].Entry.ID != "K1" {
			t.Fatalf("tag filter: %v", hitIDs(hits))
		}
		hits, _ = s.Knowledge.Search(c, domain.KnowledgeQuery{Limit: 2})
		if len(hits) != 2 || hits[0].Entry.ID != "K3" {
			t.Fatalf("empty query = newest first, limited: %v", hitIDs(hits))
		}
		e, _ := s.Knowledge.Get(c, "K3")
		e.TaskID = "T"
		_ = s.Knowledge.Update(c, e)
		if hits, _ = s.Knowledge.Search(c, domain.KnowledgeQuery{TaskID: "T"}); len(hits) != 3 {
			t.Fatalf("update must move the entry to T: %v", hitIDs(hits))
		}
		if err := s.Knowledge.Delete(c, "K3"); err != nil {
			t.Fatal(err)
		}
		if err := s.Knowledge.Delete(c, "K3"); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("delete missing: %v", err)
		}
	})
}

func hitIDs(hs []domain.KnowledgeHit) []domain.KnowledgeID {
	out := make([]domain.KnowledgeID, len(hs))
	for i, h := range hs {
		out[i] = h.Entry.ID
	}
	return out
}

// TestSQLiteSurvivesReopen is the point of the SQLite store.
func TestSQLiteSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "ritam.db")
	c := ctx()
	db, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	s := db.Storage()
	n, _ := s.Tasks.NextSequence(c)
	_ = s.Tasks.Create(c, &domain.Task{ID: domain.FormatTaskID(n), Title: "kept", State: domain.TaskCompleted})
	_ = s.Messages.Append(c, &domain.Message{ID: "M1", TaskID: domain.FormatTaskID(n), Content: "hi"})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s = db.Storage()
	got, err := s.Tasks.Get(c, "TASK-001")
	if err != nil || got.Title != "kept" || got.State != domain.TaskCompleted {
		t.Fatalf("task lost across reopen: %v %+v", err, got)
	}
	if msgs, _ := s.Messages.ListByTask(c, "TASK-001"); len(msgs) != 1 || msgs[0].Content != "hi" {
		t.Fatal("messages lost across reopen")
	}
	if n, _ := s.Tasks.NextSequence(c); n != 2 {
		t.Fatalf("task numbering must continue after a restart, got %d", n)
	}
}

func TestSQLiteRefusesANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ritam.db")
	db, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = db.db.Exec("PRAGMA user_version = 99")
	_ = db.Close()
	if _, err := OpenSQLite(path); err == nil {
		t.Fatal("a database from a newer build must not be opened (and silently mangled)")
	}
}

func TestSQLiteAllowsOneRitamPerDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ritam.db")
	first, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = OpenSQLite(path)
	var locked *ErrLocked
	if !errors.As(err, &locked) {
		t.Fatalf("a second ritam on the same database must be refused, got %v", err)
	}
	if locked.Holder == "" {
		t.Error("the refusal should name the pid holding the database")
	}
	_ = first.Close()
	again, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("closing must release the lock: %v", err)
	}
	_ = again.Close()
}

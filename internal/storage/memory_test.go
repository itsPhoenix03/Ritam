package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

func ctx() context.Context { return context.Background() }

func TestMemTaskRepository_CRUDAndNotFound(t *testing.T) {
	c := ctx()
	r := NewMemTaskRepository()

	task := &domain.Task{ID: domain.TaskID("TASK-001"), Title: "first", Participants: []domain.AgentID{"a"}}
	if err := r.Create(c, task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Create(c, task); !errors.Is(err, ports.ErrAlreadyExists) {
		t.Fatalf("Create duplicate: got %v, want ErrAlreadyExists", err)
	}

	got, err := r.Get(c, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "first" {
		t.Fatalf("Get: title = %q", got.Title)
	}

	if _, err := r.Get(c, domain.TaskID("nope")); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Get missing: got %v, want ErrNotFound", err)
	}

	missing := &domain.Task{ID: domain.TaskID("TASK-999")}
	if err := r.Update(c, missing); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Update missing: got %v, want ErrNotFound", err)
	}

	got.Title = "updated"
	if err := r.Update(c, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got2, _ := r.Get(c, task.ID)
	if got2.Title != "updated" {
		t.Fatalf("Update did not persist: %q", got2.Title)
	}
}

func TestMemTaskRepository_CopyIsolation(t *testing.T) {
	c := ctx()
	r := NewMemTaskRepository()
	task := &domain.Task{ID: domain.TaskID("TASK-001"), Title: "orig", Participants: []domain.AgentID{"a"}}
	if err := r.Create(c, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Mutating the value passed to Create must not affect stored state.
	task.Title = "mutated-by-caller"
	task.Participants[0] = "mutated"

	got, _ := r.Get(c, task.ID)
	if got.Title != "orig" || got.Participants[0] != "a" {
		t.Fatalf("store was mutated via caller's reference: %+v", got)
	}

	// Mutating a returned value must not affect stored state either.
	got.Title = "mutated-by-reader"
	got.Participants[0] = "mutated-again"
	got2, _ := r.Get(c, task.ID)
	if got2.Title != "orig" || got2.Participants[0] != "a" {
		t.Fatalf("store was mutated via returned value: %+v", got2)
	}
}

func TestMemTaskRepository_ListOrderAndNextSequence(t *testing.T) {
	c := ctx()
	r := NewMemTaskRepository()
	for i := 1; i <= 3; i++ {
		n, err := r.NextSequence(c)
		if err != nil {
			t.Fatalf("NextSequence: %v", err)
		}
		if n != i {
			t.Fatalf("NextSequence: got %d, want %d", n, i)
		}
		id := domain.FormatTaskID(n)
		if err := r.Create(c, &domain.Task{ID: id, Title: string(id)}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	list, err := r.List(c)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("List: got %d tasks", len(list))
	}
	want := []domain.TaskID{"TASK-001", "TASK-002", "TASK-003"}
	for i, id := range want {
		if list[i].ID != id {
			t.Fatalf("List order: at %d got %s want %s", i, list[i].ID, id)
		}
	}
}

func TestMemMessageRepository(t *testing.T) {
	c := ctx()
	r := NewMemMessageRepository()
	task := domain.TaskID("TASK-001")

	for i := 0; i < 3; i++ {
		m := &domain.Message{
			ID:        domain.MessageID(domain.NewMessageID()),
			TaskID:    task,
			Content:   "hello",
			Timestamp: time.Now(),
			Metadata:  map[string]string{"i": "x"},
		}
		if err := r.Append(c, m); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	list, err := r.ListByTask(c, task)
	if err != nil {
		t.Fatalf("ListByTask: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("ListByTask: got %d", len(list))
	}

	count, err := r.CountByTask(c, task)
	if err != nil || count != 3 {
		t.Fatalf("CountByTask: got %d, err %v", count, err)
	}

	first := list[0]
	first.Content = "mutated"
	first.Metadata["i"] = "mutated"
	list2, _ := r.ListByTask(c, task)
	if list2[0].Content != "hello" || list2[0].Metadata["i"] != "x" {
		t.Fatalf("ListByTask returned a shared reference: %+v", list2[0])
	}

	if _, err := r.Get(c, domain.MessageID("nope")); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Get missing: got %v", err)
	}
	dupID := list[0].ID
	if err := r.Append(c, &domain.Message{ID: dupID, TaskID: task}); !errors.Is(err, ports.ErrAlreadyExists) {
		t.Fatalf("Append duplicate: got %v", err)
	}
}

func TestMemEventRepository(t *testing.T) {
	c := ctx()
	r := NewMemEventRepository()
	task := domain.TaskID("TASK-001")

	for i := 0; i < 3; i++ {
		e := domain.Event{
			ID:      domain.EventID(domain.NewEventID()),
			Type:    domain.EvtTaskCreated,
			TaskID:  task,
			Details: map[string]string{"n": "0"},
		}
		if err := r.Append(c, e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	// Also add an event for a different task to make sure ListByTask scopes.
	if err := r.Append(c, domain.Event{ID: "EVT-other", TaskID: "TASK-002"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	list, err := r.ListByTask(c, task)
	if err != nil || len(list) != 3 {
		t.Fatalf("ListByTask: got %d, err %v", len(list), err)
	}
	list[0].Details["n"] = "mutated"
	list2, _ := r.ListByTask(c, task)
	if list2[0].Details["n"] != "0" {
		t.Fatalf("ListByTask returned a shared reference: %+v", list2[0])
	}

	all, err := r.ListAll(c)
	if err != nil || len(all) != 4 {
		t.Fatalf("ListAll: got %d, err %v", len(all), err)
	}
}

func TestMemDecisionRepository(t *testing.T) {
	c := ctx()
	r := NewMemDecisionRepository()
	task := domain.TaskID("TASK-001")

	n, err := r.NextSequence(c)
	if err != nil || n != 1 {
		t.Fatalf("NextSequence: %d, %v", n, err)
	}
	n2, _ := r.NextSequence(c)
	if n2 != 2 {
		t.Fatalf("NextSequence not monotonic: %d", n2)
	}

	d := &domain.Decision{ID: domain.FormatDecisionID(n), TaskID: task, Decision: "do X", Evidence: []string{"e1"}}
	if err := r.Create(c, d); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Create(c, d); !errors.Is(err, ports.ErrAlreadyExists) {
		t.Fatalf("Create duplicate: %v", err)
	}

	got, err := r.Get(c, d.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Evidence[0] = "mutated"
	got2, _ := r.Get(c, d.ID)
	if got2.Evidence[0] != "e1" {
		t.Fatalf("Get returned shared reference: %+v", got2)
	}

	missing := &domain.Decision{ID: "D-999"}
	if err := r.Update(c, missing); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Update missing: %v", err)
	}

	list, err := r.ListByTask(c, task)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListByTask: %d, %v", len(list), err)
	}
}

func TestMemArtifactRepository(t *testing.T) {
	c := ctx()
	r := NewMemArtifactRepository()
	task := domain.TaskID("TASK-001")

	a := &domain.Artifact{ID: domain.NewArtifactID(), TaskID: task, Location: "path/a"}
	if err := r.Create(c, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Create(c, a); !errors.Is(err, ports.ErrAlreadyExists) {
		t.Fatalf("Create duplicate: %v", err)
	}

	got, err := r.Get(c, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Location = "mutated"
	got2, _ := r.Get(c, a.ID)
	if got2.Location != "path/a" {
		t.Fatalf("Get returned shared reference: %+v", got2)
	}

	if _, err := r.Get(c, domain.ArtifactID("nope")); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Get missing: %v", err)
	}

	list, err := r.ListByTask(c, task)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListByTask: %d, %v", len(list), err)
	}
}

func TestMemKnowledgeRepository_CRUD(t *testing.T) {
	c := ctx()
	r := NewMemKnowledgeRepository()
	e := &domain.KnowledgeEntry{ID: domain.NewKnowledgeID(), Category: domain.KnowledgeAgentNote, Title: "t", Body: "b", Tags: []string{"x"}}
	if err := r.Put(c, e); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := r.Put(c, e); !errors.Is(err, ports.ErrAlreadyExists) {
		t.Fatalf("Put duplicate: %v", err)
	}

	got, err := r.Get(c, e.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Tags[0] = "mutated"
	got2, _ := r.Get(c, e.ID)
	if got2.Tags[0] != "x" {
		t.Fatalf("Get returned shared reference: %+v", got2)
	}

	got.Body = "updated"
	if err := r.Update(c, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got3, _ := r.Get(c, e.ID)
	if got3.Body != "updated" {
		t.Fatalf("Update did not persist")
	}

	if err := r.Delete(c, e.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(c, e.ID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}
	if err := r.Delete(c, e.ID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestMemKnowledgeRepository_Search(t *testing.T) {
	c := ctx()
	r := NewMemKnowledgeRepository()
	base := time.Now().Add(-time.Hour)

	mk := func(id, title, body string, cat domain.KnowledgeCategory, taskID domain.TaskID, agentID domain.AgentID, tags []string, at time.Time) *domain.KnowledgeEntry {
		e := &domain.KnowledgeEntry{
			ID: domain.KnowledgeID(id), Category: cat, TaskID: taskID, AgentID: agentID,
			Title: title, Body: body, Tags: tags, CreatedAt: at, UpdatedAt: at,
		}
		if err := r.Put(c, e); err != nil {
			t.Fatalf("Put %s: %v", id, err)
		}
		return e
	}

	mk("KN-1", "database migration plan", "we will migrate the database", domain.KnowledgeArchitecture, "TASK-001", "opus", []string{"db"}, base)
	mk("KN-2", "unrelated note", "database appears here too, once", domain.KnowledgeAgentNote, "TASK-001", "codex", nil, base.Add(time.Minute))
	mk("KN-3", "project context", "general project info", domain.KnowledgeProjectContext, "", "", []string{"overview"}, base.Add(2*time.Minute))
	mk("KN-4", "other task db note", "database notes for a different task", domain.KnowledgeInvestigation, "TASK-002", "opus", []string{"db"}, base.Add(3*time.Minute))

	// Text search: KN-1 should outrank KN-2 (title match beats body-only match).
	hits, err := r.Search(c, domain.KnowledgeQuery{Text: "database"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("Search text: got %d hits, want 3", len(hits))
	}
	if hits[0].Entry.ID != "KN-1" {
		t.Fatalf("Search text: top hit = %s, want KN-1 (title match should outrank body-only)", hits[0].Entry.ID)
	}

	// TaskID filter.
	hits, err = r.Search(c, domain.KnowledgeQuery{TaskID: "TASK-001"})
	if err != nil || len(hits) != 2 {
		t.Fatalf("Search TaskID: got %d, err %v", len(hits), err)
	}

	// AgentID filter.
	hits, err = r.Search(c, domain.KnowledgeQuery{AgentID: "opus"})
	if err != nil || len(hits) != 2 {
		t.Fatalf("Search AgentID: got %d, err %v", len(hits), err)
	}

	// Category filter.
	hits, err = r.Search(c, domain.KnowledgeQuery{Categories: []domain.KnowledgeCategory{domain.KnowledgeArchitecture, domain.KnowledgeProjectContext}})
	if err != nil || len(hits) != 2 {
		t.Fatalf("Search Categories: got %d, err %v", len(hits), err)
	}

	// Tags filter.
	hits, err = r.Search(c, domain.KnowledgeQuery{Tags: []string{"db"}})
	if err != nil || len(hits) != 2 {
		t.Fatalf("Search Tags: got %d, err %v", len(hits), err)
	}

	// AND semantics: text + taskID narrows further.
	hits, err = r.Search(c, domain.KnowledgeQuery{Text: "database", TaskID: "TASK-001"})
	if err != nil || len(hits) != 2 {
		t.Fatalf("Search AND: got %d, err %v", len(hits), err)
	}

	// No match.
	hits, err = r.Search(c, domain.KnowledgeQuery{Text: "nonexistentterm"})
	if err != nil || len(hits) != 0 {
		t.Fatalf("Search no match: got %d, err %v", len(hits), err)
	}

	// Limit truncates.
	hits, err = r.Search(c, domain.KnowledgeQuery{Limit: 1})
	if err != nil || len(hits) != 1 {
		t.Fatalf("Search Limit: got %d, err %v", len(hits), err)
	}
	// With no text, score ties at 1 so ordering falls back to CreatedAt desc;
	// KN-4 is newest.
	if hits[0].Entry.ID != "KN-4" {
		t.Fatalf("Search Limit: got %s, want newest entry KN-4", hits[0].Entry.ID)
	}
}

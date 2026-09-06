// Package knowledge is the shared-knowledge layer that sits above
// ports.KnowledgeRepository. Its purpose is the one described in the
// package's domain model: let agents (and the orchestrator) retrieve
// relevant context for a task instead of replaying the whole project
// history.
package knowledge

import (
	"context"
	"fmt"
	"sort"
	"time"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// Store wraps a ports.KnowledgeRepository with validation and the
// convenience recording helpers used by the orchestrator layer.
type Store struct {
	repo ports.KnowledgeRepository
}

// New constructs a Store over the given repository.
func New(repo ports.KnowledgeRepository) *Store {
	return &Store{repo: repo}
}

// validate rejects entries the store must never persist: an unknown category
// or a missing title. Everything else (body, tags, refs, scoping) is
// optional.
func validate(e *domain.KnowledgeEntry) error {
	if e.Title == "" {
		return fmt.Errorf("knowledge entry: title is required")
	}
	known := false
	for _, c := range domain.AllKnowledgeCategories() {
		if c == e.Category {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("knowledge entry: unknown category %q", e.Category)
	}
	return nil
}

// Put stores a new entry (put()), assigning an id and timestamps if the
// caller did not already set an id.
func (s *Store) Put(ctx context.Context, e *domain.KnowledgeEntry) (*domain.KnowledgeEntry, error) {
	if err := validate(e); err != nil {
		return nil, err
	}
	cp := e.Clone()
	if cp.ID == "" {
		cp.ID = domain.NewKnowledgeID()
	}
	now := time.Now()
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	cp.UpdatedAt = now
	if err := s.repo.Put(ctx, cp); err != nil {
		return nil, err
	}
	return cp.Clone(), nil
}

// Get retrieves an entry by id (get()).
func (s *Store) Get(ctx context.Context, id domain.KnowledgeID) (*domain.KnowledgeEntry, error) {
	return s.repo.Get(ctx, id)
}

// Update modifies an existing entry in place (update()), bumping UpdatedAt.
// The entry must already exist (created via Put) and pass the same
// validation.
func (s *Store) Update(ctx context.Context, e *domain.KnowledgeEntry) (*domain.KnowledgeEntry, error) {
	if err := validate(e); err != nil {
		return nil, err
	}
	existing, err := s.repo.Get(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	cp := e.Clone()
	cp.CreatedAt = existing.CreatedAt
	cp.UpdatedAt = time.Now()
	if err := s.repo.Update(ctx, cp); err != nil {
		return nil, err
	}
	return cp.Clone(), nil
}

// Delete removes an entry by id (delete()).
func (s *Store) Delete(ctx context.Context, id domain.KnowledgeID) error {
	return s.repo.Delete(ctx, id)
}

// Search runs a knowledge query (search()) and returns scored hits.
func (s *Store) Search(ctx context.Context, q domain.KnowledgeQuery) ([]domain.KnowledgeHit, error) {
	return s.repo.Search(ctx, q)
}

// record is the shared implementation behind the Record* convenience
// helpers: it builds and stores an entry of the given category.
func (s *Store) record(
	ctx context.Context,
	category domain.KnowledgeCategory,
	taskID domain.TaskID,
	agentID domain.AgentID,
	title, body string,
	refs []string,
) (*domain.KnowledgeEntry, error) {
	return s.Put(ctx, &domain.KnowledgeEntry{
		Category: category,
		TaskID:   taskID,
		AgentID:  agentID,
		Title:    title,
		Body:     body,
		Refs:     refs,
	})
}

// RecordInvestigation stores an agent's investigation findings for a task.
func (s *Store) RecordInvestigation(ctx context.Context, taskID domain.TaskID, agentID domain.AgentID, title, body string) (*domain.KnowledgeEntry, error) {
	return s.record(ctx, domain.KnowledgeInvestigation, taskID, agentID, title, body, nil)
}

// RecordDecision stores a durable decision as shared knowledge so it shows up
// in context retrieval alongside investigations and notes. The body folds in
// the decision text, its reasoning and evidence; Refs points back at the
// decision id.
func (s *Store) RecordDecision(ctx context.Context, d *domain.Decision) (*domain.KnowledgeEntry, error) {
	body := fmt.Sprintf("Decision: %s\n\nReasoning: %s", d.Decision, d.Reasoning)
	if len(d.Evidence) > 0 {
		body += "\n\nEvidence:"
		for _, ev := range d.Evidence {
			body += "\n- " + ev
		}
	}
	title := fmt.Sprintf("Decision %s: %s", d.ID, d.Decision)
	return s.record(ctx, domain.KnowledgeDecision, d.TaskID, d.Author, title, body, []string{string(d.ID)})
}

// RecordTestResult stores the outcome of a test run.
func (s *Store) RecordTestResult(ctx context.Context, taskID domain.TaskID, agentID domain.AgentID, title, body string) (*domain.KnowledgeEntry, error) {
	return s.record(ctx, domain.KnowledgeTestResult, taskID, agentID, title, body, nil)
}

// RecordAgentNote stores a free-form note an agent wants to persist for later
// retrieval.
func (s *Store) RecordAgentNote(ctx context.Context, taskID domain.TaskID, agentID domain.AgentID, title, body string) (*domain.KnowledgeEntry, error) {
	return s.record(ctx, domain.KnowledgeAgentNote, taskID, agentID, title, body, nil)
}

// ContextFor is the "retrieve relevant knowledge instead of the whole
// history" entry point. It searches entries scoped to taskID plus
// project-wide entries (TaskID == ""), ranked by relevance to query, and
// returns at most limit hits. This is what an agent's context-assembly step
// should call instead of reading the full message/event history.
func (s *Store) ContextFor(ctx context.Context, taskID domain.TaskID, query string, limit int) ([]domain.KnowledgeHit, error) {
	taskHits, err := s.repo.Search(ctx, domain.KnowledgeQuery{Text: query, TaskID: taskID})
	if err != nil {
		return nil, err
	}
	projectHits, err := s.repo.Search(ctx, domain.KnowledgeQuery{Text: query})
	if err != nil {
		return nil, err
	}

	seen := make(map[domain.KnowledgeID]struct{}, len(taskHits))
	all := make([]domain.KnowledgeHit, 0, len(taskHits)+len(projectHits))
	for _, h := range taskHits {
		seen[h.Entry.ID] = struct{}{}
		all = append(all, h)
	}
	for _, h := range projectHits {
		// Project-wide entries only (task-scoped entries belonging to other
		// tasks were already excluded by the TaskID filter above and must not
		// leak in here); skip anything already collected from taskHits.
		if h.Entry.TaskID != "" {
			continue
		}
		if _, dup := seen[h.Entry.ID]; dup {
			continue
		}
		all = append(all, h)
	}

	sortHitsByScoreThenRecency(all)
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// sortHitsByScoreThenRecency orders hits by descending score, breaking ties
// by newest first. It mirrors the ordering the repository's own Search
// applies, needed here because ContextFor merges two separate searches.
func sortHitsByScoreThenRecency(hits []domain.KnowledgeHit) {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Entry.CreatedAt.After(hits[j].Entry.CreatedAt)
	})
}

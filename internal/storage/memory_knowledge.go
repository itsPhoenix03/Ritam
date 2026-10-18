package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// MemKnowledgeRepository is an in-memory, goroutine-safe
// ports.KnowledgeRepository.
//
// Search here is deliberately naive: a case-insensitive substring scan with a
// simple term-frequency-ish score. It exists so the rest of the scaffold has
// something to retrieve relevant context against today. A later phase can
// replace it with a SQLite FTS5 (or vector) backed implementation behind the
// exact same ports.KnowledgeRepository interface; no caller would need to
// change.
type MemKnowledgeRepository struct {
	mu      sync.RWMutex
	entries map[domain.KnowledgeID]*domain.KnowledgeEntry
}

// NewMemKnowledgeRepository constructs an empty knowledge repository.
func NewMemKnowledgeRepository() *MemKnowledgeRepository {
	return &MemKnowledgeRepository{entries: make(map[domain.KnowledgeID]*domain.KnowledgeEntry)}
}

// Put stores a new knowledge entry. It fails if the id is already taken; use
// Update to modify an existing entry.
func (r *MemKnowledgeRepository) Put(_ context.Context, e *domain.KnowledgeEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[e.ID]; ok {
		return fmt.Errorf("%w: knowledge entry %s", ports.ErrAlreadyExists, e.ID)
	}
	r.entries[e.ID] = e.Clone()
	return nil
}

// Get returns a deep copy of the entry with the given id.
func (r *MemKnowledgeRepository) Get(_ context.Context, id domain.KnowledgeID) (*domain.KnowledgeEntry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	if !ok {
		return nil, fmt.Errorf("%w: knowledge entry %s", ports.ErrNotFound, id)
	}
	return e.Clone(), nil
}

// Update overwrites an existing entry. It fails if the entry does not exist.
func (r *MemKnowledgeRepository) Update(_ context.Context, e *domain.KnowledgeEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[e.ID]; !ok {
		return fmt.Errorf("%w: knowledge entry %s", ports.ErrNotFound, e.ID)
	}
	r.entries[e.ID] = e.Clone()
	return nil
}

// Delete removes an entry. It fails if the entry does not exist.
func (r *MemKnowledgeRepository) Delete(_ context.Context, id domain.KnowledgeID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[id]; !ok {
		return fmt.Errorf("%w: knowledge entry %s", ports.ErrNotFound, id)
	}
	delete(r.entries, id)
	return nil
}

// Search implements domain.KnowledgeQuery semantics: every non-empty filter
// (Text, Categories, TaskID, AgentID, Tags) must match, i.e. filters combine
// with AND. Text is a case-insensitive substring match over the title, body
// and tags. Results are sorted by score descending, then by CreatedAt
// descending (newest first) as a tiebreak, and truncated to Limit when
// Limit > 0.
func (r *MemKnowledgeRepository) Search(_ context.Context, q domain.KnowledgeQuery) ([]domain.KnowledgeHit, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var hits []domain.KnowledgeHit
	for _, e := range r.entries {
		if score, ok := matchKnowledge(e, q); ok {
			hits = append(hits, domain.KnowledgeHit{Entry: e.Clone(), Score: score})
		}
	}
	return rankKnowledge(hits, q.Limit), nil
}

// matchKnowledge applies every filter of q to e and, if all pass, returns the
// entry's relevance. Shared by both backends so search ranks identically.
func matchKnowledge(e *domain.KnowledgeEntry, q domain.KnowledgeQuery) (float64, bool) {
	if q.TaskID != "" && e.TaskID != q.TaskID {
		return 0, false
	}
	if q.AgentID != "" && e.AgentID != q.AgentID {
		return 0, false
	}
	if len(q.Categories) > 0 && !containsCategory(q.Categories, e.Category) {
		return 0, false
	}
	if len(q.Tags) > 0 && !hasAllTags(e.Tags, q.Tags) {
		return 0, false
	}
	return knowledgeScore(e, strings.ToLower(strings.TrimSpace(q.Text)))
}

// rankKnowledge sorts hits by score, newest first on ties, and truncates to
// limit when limit > 0.
func rankKnowledge(hits []domain.KnowledgeHit, limit int) []domain.KnowledgeHit {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Entry.CreatedAt.After(hits[j].Entry.CreatedAt)
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// knowledgeScore reports whether entry e matches the (already lower-cased)
// text query, and if so its relevance score in [0, 1). An empty text query
// matches everything with a flat score of 1, so ordering falls back to
// recency. Title hits are weighted 3x, tag hits 2x and body hits 1x; the raw
// weighted hit count is squashed into (0, 1) so more matches always score
// higher without needing an upper bound.
func knowledgeScore(e *domain.KnowledgeEntry, text string) (float64, bool) {
	if text == "" {
		return 1, true
	}
	title := strings.ToLower(e.Title)
	body := strings.ToLower(e.Body)

	titleHits := strings.Count(title, text)
	bodyHits := strings.Count(body, text)
	tagHits := 0
	for _, tag := range e.Tags {
		tagHits += strings.Count(strings.ToLower(tag), text)
	}

	if titleHits == 0 && bodyHits == 0 && tagHits == 0 {
		return 0, false
	}
	weighted := float64(titleHits)*3 + float64(tagHits)*2 + float64(bodyHits)
	return weighted / (weighted + 1), true
}

func containsCategory(cats []domain.KnowledgeCategory, c domain.KnowledgeCategory) bool {
	for _, want := range cats {
		if want == c {
			return true
		}
	}
	return false
}

// hasAllTags reports whether entryTags contains every tag in want
// (case-insensitive, AND semantics).
func hasAllTags(entryTags, want []string) bool {
	set := make(map[string]struct{}, len(entryTags))
	for _, t := range entryTags {
		set[strings.ToLower(t)] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[strings.ToLower(w)]; !ok {
			return false
		}
	}
	return true
}

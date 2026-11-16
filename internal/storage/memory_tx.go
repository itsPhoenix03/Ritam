package storage

import (
	"context"
	"errors"
	"maps"
	"sync"
	"sync/atomic"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// The in-memory store gives the same transactions as SQLite with a
// store-wide gate and a snapshot. Every call through the Storage that
// NewInMemory returns passes the gate; a transaction holds it for its whole
// length, so no other goroutine sees its writes before it ends or slips a
// write in that a rollback would then wipe out. A failing transaction puts
// back the snapshot taken when it began.
//
// Snapshots are shallow: the repositories replace a stored record on every
// write and never change one in place, so copying the maps and slice
// headers is enough to restore them.

// memStore holds the repositories behind one gate.
type memStore struct {
	gate          sync.Mutex
	tasks         *MemTaskRepository
	messages      *MemMessageRepository
	events        *MemEventRepository
	decisions     *MemDecisionRepository
	disagreements *MemDisagreementRepository
	knowledge     *MemKnowledgeRepository
	artifacts     *MemArtifactRepository
	deliveries    *MemDeliveryRepository
}

// memDoor is how a call gets to the repositories: through the gate from
// outside a transaction, straight in from inside one.
type memDoor func() (release func(), err error)

// errTxDone is returned by a repository of a transaction that has ended:
// writing through it would bypass the gate.
var errTxDone = errors.New("storage: transaction has already ended")

func newMemStore() *memStore {
	return &memStore{
		tasks:         NewMemTaskRepository(),
		messages:      NewMemMessageRepository(),
		events:        NewMemEventRepository(),
		decisions:     NewMemDecisionRepository(),
		disagreements: NewMemDisagreementRepository(),
		knowledge:     NewMemKnowledgeRepository(),
		artifacts:     NewMemArtifactRepository(),
		deliveries:    NewMemDeliveryRepository(),
	}
}

func (s *memStore) outerDoor() (func(), error) {
	s.gate.Lock()
	return s.gate.Unlock, nil
}

// storage builds the repositories behind door, with tx as their transactor.
func (s *memStore) storage(door memDoor, tx ports.Transactor) *ports.Storage {
	return &ports.Storage{
		Tasks:         &gatedTasks{door, s.tasks},
		Messages:      &gatedMessages{door, s.messages},
		Events:        &gatedEvents{door, s.events},
		Decisions:     &gatedDecisions{door, s.decisions},
		Disagreements: &gatedDisagreements{door, s.disagreements},
		Knowledge:     &gatedKnowledge{door, s.knowledge},
		Artifacts:     &gatedArtifacts{door, s.artifacts},
		Deliveries:    &gatedDeliveries{door, s.deliveries},
		Tx:            tx,
	}
}

// memTransactor opens a top-level transaction.
type memTransactor struct{ s *memStore }

func (t *memTransactor) WithTx(ctx context.Context, fn func(*ports.Storage) error) error {
	t.s.gate.Lock()
	defer t.s.gate.Unlock()
	return t.s.run(ctx, fn)
}

// memNested is the transactor of a transaction's own Storage: the gate is
// already held, so nesting only takes another snapshot.
type memNested struct{ s *memStore }

func (t *memNested) WithTx(ctx context.Context, fn func(*ports.Storage) error) error {
	return t.s.run(ctx, fn)
}

// run calls fn with the gate held, putting the snapshot back unless fn
// returns nil.
func (s *memStore) run(ctx context.Context, fn func(*ports.Storage) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	restore := s.snapshot()
	var open atomic.Bool
	open.Store(true)
	door := func() (func(), error) {
		if !open.Load() {
			return nil, errTxDone
		}
		return func() {}, nil
	}
	committed := false
	defer func() {
		open.Store(false)
		if !committed {
			restore()
		}
	}()
	if err := fn(s.storage(door, &memNested{s})); err != nil {
		return err
	}
	committed = true
	return nil
}

// snapshot records every repository's contents and returns what puts them
// back.
func (s *memStore) snapshot() (restore func()) {
	undo := []func(){
		s.tasks.snapshot(), s.messages.snapshot(), s.events.snapshot(), s.decisions.snapshot(),
		s.disagreements.snapshot(), s.knowledge.snapshot(), s.artifacts.snapshot(), s.deliveries.snapshot(),
	}
	return func() {
		for _, u := range undo {
			u()
		}
	}
}

func (r *MemTaskRepository) snapshot() func() {
	r.mu.RLock()
	tasks, order, seq := maps.Clone(r.tasks), r.order, r.seq
	r.mu.RUnlock()
	return func() {
		r.mu.Lock()
		r.tasks, r.order, r.seq = tasks, order, seq
		r.mu.Unlock()
	}
}

func (r *MemMessageRepository) snapshot() func() {
	r.mu.RLock()
	byID, byTask := maps.Clone(r.byID), maps.Clone(r.byTask)
	r.mu.RUnlock()
	return func() {
		r.mu.Lock()
		r.byID, r.byTask = byID, byTask
		r.mu.Unlock()
	}
}

func (r *MemEventRepository) snapshot() func() {
	r.mu.RLock()
	all, byID := r.all, maps.Clone(r.byID)
	r.mu.RUnlock()
	return func() {
		r.mu.Lock()
		r.all, r.byID = all, byID
		r.mu.Unlock()
	}
}

func (r *MemDecisionRepository) snapshot() func() {
	r.mu.RLock()
	byID, byTask, seq := maps.Clone(r.byID), maps.Clone(r.byTask), r.seq
	r.mu.RUnlock()
	return func() {
		r.mu.Lock()
		r.byID, r.byTask, r.seq = byID, byTask, seq
		r.mu.Unlock()
	}
}

func (r *MemDisagreementRepository) snapshot() func() {
	r.mu.RLock()
	byID, byTask := maps.Clone(r.byID), maps.Clone(r.byTask)
	r.mu.RUnlock()
	return func() {
		r.mu.Lock()
		r.byID, r.byTask = byID, byTask
		r.mu.Unlock()
	}
}

func (r *MemKnowledgeRepository) snapshot() func() {
	r.mu.RLock()
	entries := maps.Clone(r.entries)
	r.mu.RUnlock()
	return func() {
		r.mu.Lock()
		r.entries = entries
		r.mu.Unlock()
	}
}

func (r *MemArtifactRepository) snapshot() func() {
	r.mu.RLock()
	byID, byTask := maps.Clone(r.byID), maps.Clone(r.byTask)
	r.mu.RUnlock()
	return func() {
		r.mu.Lock()
		r.byID, r.byTask = byID, byTask
		r.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// gated repositories
// ---------------------------------------------------------------------------

func through[T any](d memDoor, f func() (T, error)) (T, error) {
	release, err := d()
	if err != nil {
		var zero T
		return zero, err
	}
	defer release()
	return f()
}

func through0(d memDoor, f func() error) error {
	release, err := d()
	if err != nil {
		return err
	}
	defer release()
	return f()
}

type gatedTasks struct {
	d memDoor
	r *MemTaskRepository
}

func (g *gatedTasks) Create(ctx context.Context, t *domain.Task) error {
	return through0(g.d, func() error { return g.r.Create(ctx, t) })
}
func (g *gatedTasks) Update(ctx context.Context, t *domain.Task) error {
	return through0(g.d, func() error { return g.r.Update(ctx, t) })
}
func (g *gatedTasks) Get(ctx context.Context, id domain.TaskID) (*domain.Task, error) {
	return through(g.d, func() (*domain.Task, error) { return g.r.Get(ctx, id) })
}
func (g *gatedTasks) List(ctx context.Context) ([]*domain.Task, error) {
	return through(g.d, func() ([]*domain.Task, error) { return g.r.List(ctx) })
}
func (g *gatedTasks) NextSequence(ctx context.Context) (int, error) {
	return through(g.d, func() (int, error) { return g.r.NextSequence(ctx) })
}

type gatedMessages struct {
	d memDoor
	r *MemMessageRepository
}

func (g *gatedMessages) Append(ctx context.Context, m *domain.Message) error {
	return through0(g.d, func() error { return g.r.Append(ctx, m) })
}
func (g *gatedMessages) Get(ctx context.Context, id domain.MessageID) (*domain.Message, error) {
	return through(g.d, func() (*domain.Message, error) { return g.r.Get(ctx, id) })
}
func (g *gatedMessages) ListByTask(ctx context.Context, id domain.TaskID) ([]*domain.Message, error) {
	return through(g.d, func() ([]*domain.Message, error) { return g.r.ListByTask(ctx, id) })
}
func (g *gatedMessages) CountByTask(ctx context.Context, id domain.TaskID) (int, error) {
	return through(g.d, func() (int, error) { return g.r.CountByTask(ctx, id) })
}

type gatedEvents struct {
	d memDoor
	r *MemEventRepository
}

func (g *gatedEvents) Append(ctx context.Context, e domain.Event) error {
	return through0(g.d, func() error { return g.r.Append(ctx, e) })
}
func (g *gatedEvents) ListByTask(ctx context.Context, id domain.TaskID) ([]domain.Event, error) {
	return through(g.d, func() ([]domain.Event, error) { return g.r.ListByTask(ctx, id) })
}
func (g *gatedEvents) ListAll(ctx context.Context) ([]domain.Event, error) {
	return through(g.d, func() ([]domain.Event, error) { return g.r.ListAll(ctx) })
}

type gatedDecisions struct {
	d memDoor
	r *MemDecisionRepository
}

func (g *gatedDecisions) Create(ctx context.Context, d *domain.Decision) error {
	return through0(g.d, func() error { return g.r.Create(ctx, d) })
}
func (g *gatedDecisions) Update(ctx context.Context, d *domain.Decision) error {
	return through0(g.d, func() error { return g.r.Update(ctx, d) })
}
func (g *gatedDecisions) Get(ctx context.Context, id domain.DecisionID) (*domain.Decision, error) {
	return through(g.d, func() (*domain.Decision, error) { return g.r.Get(ctx, id) })
}
func (g *gatedDecisions) ListByTask(ctx context.Context, id domain.TaskID) ([]*domain.Decision, error) {
	return through(g.d, func() ([]*domain.Decision, error) { return g.r.ListByTask(ctx, id) })
}
func (g *gatedDecisions) NextSequence(ctx context.Context) (int, error) {
	return through(g.d, func() (int, error) { return g.r.NextSequence(ctx) })
}

type gatedDisagreements struct {
	d memDoor
	r *MemDisagreementRepository
}

func (g *gatedDisagreements) Create(ctx context.Context, d *domain.Disagreement) error {
	return through0(g.d, func() error { return g.r.Create(ctx, d) })
}
func (g *gatedDisagreements) Update(ctx context.Context, d *domain.Disagreement) error {
	return through0(g.d, func() error { return g.r.Update(ctx, d) })
}
func (g *gatedDisagreements) Get(ctx context.Context, id domain.DisagreementID) (*domain.Disagreement, error) {
	return through(g.d, func() (*domain.Disagreement, error) { return g.r.Get(ctx, id) })
}
func (g *gatedDisagreements) ListByTask(ctx context.Context, id domain.TaskID) ([]*domain.Disagreement, error) {
	return through(g.d, func() ([]*domain.Disagreement, error) { return g.r.ListByTask(ctx, id) })
}
func (g *gatedDisagreements) ListOpen(ctx context.Context, id domain.TaskID) ([]*domain.Disagreement, error) {
	return through(g.d, func() ([]*domain.Disagreement, error) { return g.r.ListOpen(ctx, id) })
}

type gatedKnowledge struct {
	d memDoor
	r *MemKnowledgeRepository
}

func (g *gatedKnowledge) Put(ctx context.Context, e *domain.KnowledgeEntry) error {
	return through0(g.d, func() error { return g.r.Put(ctx, e) })
}
func (g *gatedKnowledge) Get(ctx context.Context, id domain.KnowledgeID) (*domain.KnowledgeEntry, error) {
	return through(g.d, func() (*domain.KnowledgeEntry, error) { return g.r.Get(ctx, id) })
}
func (g *gatedKnowledge) Update(ctx context.Context, e *domain.KnowledgeEntry) error {
	return through0(g.d, func() error { return g.r.Update(ctx, e) })
}
func (g *gatedKnowledge) Delete(ctx context.Context, id domain.KnowledgeID) error {
	return through0(g.d, func() error { return g.r.Delete(ctx, id) })
}
func (g *gatedKnowledge) Search(ctx context.Context, q domain.KnowledgeQuery) ([]domain.KnowledgeHit, error) {
	return through(g.d, func() ([]domain.KnowledgeHit, error) { return g.r.Search(ctx, q) })
}

type gatedArtifacts struct {
	d memDoor
	r *MemArtifactRepository
}

func (g *gatedArtifacts) Create(ctx context.Context, a *domain.Artifact) error {
	return through0(g.d, func() error { return g.r.Create(ctx, a) })
}
func (g *gatedArtifacts) Get(ctx context.Context, id domain.ArtifactID) (*domain.Artifact, error) {
	return through(g.d, func() (*domain.Artifact, error) { return g.r.Get(ctx, id) })
}
func (g *gatedArtifacts) ListByTask(ctx context.Context, id domain.TaskID) ([]*domain.Artifact, error) {
	return through(g.d, func() ([]*domain.Artifact, error) { return g.r.ListByTask(ctx, id) })
}

var (
	_ ports.TaskRepository         = (*gatedTasks)(nil)
	_ ports.MessageRepository      = (*gatedMessages)(nil)
	_ ports.EventRepository        = (*gatedEvents)(nil)
	_ ports.DecisionRepository     = (*gatedDecisions)(nil)
	_ ports.DisagreementRepository = (*gatedDisagreements)(nil)
	_ ports.KnowledgeRepository    = (*gatedKnowledge)(nil)
	_ ports.ArtifactRepository     = (*gatedArtifacts)(nil)
)

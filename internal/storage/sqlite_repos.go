package storage

import (
	"context"
	"database/sql"
	"fmt"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// nextSequence atomically increments and returns a named counter.
func nextSequence(ctx context.Context, db *sql.DB, name string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`INSERT INTO sequences(name, n) VALUES (?, 1)
		 ON CONFLICT(name) DO UPDATE SET n = n + 1
		 RETURNING n`, name).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("sqlite: next %s sequence: %w", name, err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// tasks
// ---------------------------------------------------------------------------

type sqlTasks struct{ db *sql.DB }

var _ ports.TaskRepository = (*sqlTasks)(nil)

func (r *sqlTasks) Create(ctx context.Context, t *domain.Task) error {
	data, err := encode(t)
	if err != nil {
		return err
	}
	return insert(ctx, r.db, "task", string(t.ID),
		`INSERT INTO tasks(id, state, created_at, updated_at, data) VALUES (?, ?, ?, ?, ?)`,
		string(t.ID), string(t.State), formatTime(t.CreatedAt), formatTime(t.UpdatedAt), data)
}

func (r *sqlTasks) Update(ctx context.Context, t *domain.Task) error {
	data, err := encode(t)
	if err != nil {
		return err
	}
	return update(ctx, r.db, "task", string(t.ID),
		`UPDATE tasks SET state = ?, updated_at = ?, data = ? WHERE id = ?`,
		string(t.State), formatTime(t.UpdatedAt), data, string(t.ID))
}

func (r *sqlTasks) Get(ctx context.Context, id domain.TaskID) (*domain.Task, error) {
	return getJSON[domain.Task](ctx, r.db, "task", string(id), `SELECT data FROM tasks WHERE id = ?`, string(id))
}

func (r *sqlTasks) List(ctx context.Context) ([]*domain.Task, error) {
	return listJSON[domain.Task](ctx, r.db, "tasks", `SELECT data FROM tasks ORDER BY seq`)
}

func (r *sqlTasks) NextSequence(ctx context.Context) (int, error) {
	return nextSequence(ctx, r.db, "task")
}

// ---------------------------------------------------------------------------
// messages
// ---------------------------------------------------------------------------

type sqlMessages struct{ db *sql.DB }

var _ ports.MessageRepository = (*sqlMessages)(nil)

const messageColumns = `id, task_id, from_agent, to_agent, type, content, timestamp,
	requires_response, in_reply_to, round, metadata`

func (r *sqlMessages) Append(ctx context.Context, m *domain.Message) error {
	return insert(ctx, r.db, "message", string(m.ID),
		`INSERT INTO messages(`+messageColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(m.ID), string(m.TaskID), string(m.FromAgent), string(m.ToAgent), string(m.Type),
		m.Content, formatTime(m.Timestamp), boolInt(m.RequiresResponse), string(m.InReplyTo),
		m.Round, encodeMap(m.Metadata))
}

func scanMessage(sc interface{ Scan(...any) error }) (*domain.Message, error) {
	var (
		m                               domain.Message
		id, task, from, to, typ, ts, re string
		meta                            string
		requires                        int
	)
	if err := sc.Scan(&id, &task, &from, &to, &typ, &m.Content, &ts, &requires, &re, &m.Round, &meta); err != nil {
		return nil, err
	}
	m.ID, m.TaskID = domain.MessageID(id), domain.TaskID(task)
	m.FromAgent, m.ToAgent = domain.AgentID(from), domain.AgentID(to)
	m.Type, m.InReplyTo = domain.MessageType(typ), domain.MessageID(re)
	m.RequiresResponse = requires != 0
	var err error
	if m.Timestamp, err = parseTime(ts); err != nil {
		return nil, fmt.Errorf("sqlite: message %s timestamp: %w", id, err)
	}
	if m.Metadata, err = decodeMap(meta); err != nil {
		return nil, fmt.Errorf("sqlite: message %s metadata: %w", id, err)
	}
	return &m, nil
}

func (r *sqlMessages) Get(ctx context.Context, id domain.MessageID) (*domain.Message, error) {
	m, err := scanMessage(r.db.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE id = ?`, string(id)))
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: message %s", ports.ErrNotFound, id)
	}
	return m, err
}

func (r *sqlMessages) ListByTask(ctx context.Context, id domain.TaskID) ([]*domain.Message, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE task_id = ? ORDER BY seq`, string(id))
	if err != nil {
		return nil, fmt.Errorf("sqlite: list messages: %w", err)
	}
	defer rows.Close()
	out := []*domain.Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *sqlMessages) CountByTask(ctx context.Context, id domain.TaskID) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE task_id = ?`, string(id)).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------------------
// events
// ---------------------------------------------------------------------------

type sqlEvents struct{ db *sql.DB }

var _ ports.EventRepository = (*sqlEvents)(nil)

const eventColumns = `id, type, task_id, agent_id, message_id, timestamp, summary, details`

func (r *sqlEvents) Append(ctx context.Context, e domain.Event) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO events(`+eventColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(e.ID), string(e.Type), string(e.TaskID), string(e.AgentID), string(e.MessageID),
		formatTime(e.Timestamp), e.Summary, encodeMap(e.Details))
	if err != nil {
		return fmt.Errorf("sqlite: append event: %w", err)
	}
	return nil
}

func (r *sqlEvents) list(ctx context.Context, query string, args ...any) ([]domain.Event, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list events: %w", err)
	}
	defer rows.Close()
	out := []domain.Event{}
	for rows.Next() {
		var (
			e                                  domain.Event
			id, typ, task, agent, msg, ts, det string
		)
		if err := rows.Scan(&id, &typ, &task, &agent, &msg, &ts, &e.Summary, &det); err != nil {
			return nil, err
		}
		e.ID, e.Type, e.TaskID = domain.EventID(id), domain.EventType(typ), domain.TaskID(task)
		e.AgentID, e.MessageID = domain.AgentID(agent), domain.MessageID(msg)
		if e.Timestamp, err = parseTime(ts); err != nil {
			return nil, fmt.Errorf("sqlite: event %s timestamp: %w", id, err)
		}
		if e.Details, err = decodeMap(det); err != nil {
			return nil, fmt.Errorf("sqlite: event %s details: %w", id, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *sqlEvents) ListByTask(ctx context.Context, id domain.TaskID) ([]domain.Event, error) {
	return r.list(ctx, `SELECT `+eventColumns+` FROM events WHERE task_id = ? ORDER BY seq`, string(id))
}

func (r *sqlEvents) ListAll(ctx context.Context) ([]domain.Event, error) {
	return r.list(ctx, `SELECT `+eventColumns+` FROM events ORDER BY seq`)
}

// ---------------------------------------------------------------------------
// decisions
// ---------------------------------------------------------------------------

type sqlDecisions struct{ db *sql.DB }

var _ ports.DecisionRepository = (*sqlDecisions)(nil)

func (r *sqlDecisions) Create(ctx context.Context, d *domain.Decision) error {
	data, err := encode(d)
	if err != nil {
		return err
	}
	return insert(ctx, r.db, "decision", string(d.ID),
		`INSERT INTO decisions(id, task_id, status, data) VALUES (?, ?, ?, ?)`,
		string(d.ID), string(d.TaskID), string(d.Status), data)
}

func (r *sqlDecisions) Update(ctx context.Context, d *domain.Decision) error {
	data, err := encode(d)
	if err != nil {
		return err
	}
	return update(ctx, r.db, "decision", string(d.ID),
		`UPDATE decisions SET status = ?, data = ? WHERE id = ?`, string(d.Status), data, string(d.ID))
}

func (r *sqlDecisions) Get(ctx context.Context, id domain.DecisionID) (*domain.Decision, error) {
	return getJSON[domain.Decision](ctx, r.db, "decision", string(id), `SELECT data FROM decisions WHERE id = ?`, string(id))
}

func (r *sqlDecisions) ListByTask(ctx context.Context, id domain.TaskID) ([]*domain.Decision, error) {
	return listJSON[domain.Decision](ctx, r.db, "decisions",
		`SELECT data FROM decisions WHERE task_id = ? ORDER BY seq`, string(id))
}

func (r *sqlDecisions) NextSequence(ctx context.Context) (int, error) {
	return nextSequence(ctx, r.db, "decision")
}

// ---------------------------------------------------------------------------
// disagreements
// ---------------------------------------------------------------------------

type sqlDisagreements struct{ db *sql.DB }

var _ ports.DisagreementRepository = (*sqlDisagreements)(nil)

func (r *sqlDisagreements) Create(ctx context.Context, d *domain.Disagreement) error {
	data, err := encode(d)
	if err != nil {
		return err
	}
	return insert(ctx, r.db, "disagreement", string(d.ID),
		`INSERT INTO disagreements(id, task_id, resolved, data) VALUES (?, ?, ?, ?)`,
		string(d.ID), string(d.TaskID), boolInt(d.Resolved), data)
}

func (r *sqlDisagreements) Update(ctx context.Context, d *domain.Disagreement) error {
	data, err := encode(d)
	if err != nil {
		return err
	}
	return update(ctx, r.db, "disagreement", string(d.ID),
		`UPDATE disagreements SET resolved = ?, data = ? WHERE id = ?`, boolInt(d.Resolved), data, string(d.ID))
}

func (r *sqlDisagreements) Get(ctx context.Context, id domain.DisagreementID) (*domain.Disagreement, error) {
	return getJSON[domain.Disagreement](ctx, r.db, "disagreement", string(id),
		`SELECT data FROM disagreements WHERE id = ?`, string(id))
}

func (r *sqlDisagreements) ListByTask(ctx context.Context, id domain.TaskID) ([]*domain.Disagreement, error) {
	return listJSON[domain.Disagreement](ctx, r.db, "disagreements",
		`SELECT data FROM disagreements WHERE task_id = ? ORDER BY seq`, string(id))
}

func (r *sqlDisagreements) ListOpen(ctx context.Context, id domain.TaskID) ([]*domain.Disagreement, error) {
	return listJSON[domain.Disagreement](ctx, r.db, "disagreements",
		`SELECT data FROM disagreements WHERE task_id = ? AND resolved = 0 ORDER BY seq`, string(id))
}

// ---------------------------------------------------------------------------
// knowledge
// ---------------------------------------------------------------------------

type sqlKnowledge struct{ db *sql.DB }

var _ ports.KnowledgeRepository = (*sqlKnowledge)(nil)

func (r *sqlKnowledge) Put(ctx context.Context, e *domain.KnowledgeEntry) error {
	data, err := encode(e)
	if err != nil {
		return err
	}
	return insert(ctx, r.db, "knowledge entry", string(e.ID),
		`INSERT INTO knowledge(id, task_id, agent_id, category, data) VALUES (?, ?, ?, ?, ?)`,
		string(e.ID), string(e.TaskID), string(e.AgentID), string(e.Category), data)
}

func (r *sqlKnowledge) Get(ctx context.Context, id domain.KnowledgeID) (*domain.KnowledgeEntry, error) {
	return getJSON[domain.KnowledgeEntry](ctx, r.db, "knowledge entry", string(id),
		`SELECT data FROM knowledge WHERE id = ?`, string(id))
}

func (r *sqlKnowledge) Update(ctx context.Context, e *domain.KnowledgeEntry) error {
	data, err := encode(e)
	if err != nil {
		return err
	}
	return update(ctx, r.db, "knowledge entry", string(e.ID),
		`UPDATE knowledge SET task_id = ?, agent_id = ?, category = ?, data = ? WHERE id = ?`,
		string(e.TaskID), string(e.AgentID), string(e.Category), data, string(e.ID))
}

func (r *sqlKnowledge) Delete(ctx context.Context, id domain.KnowledgeID) error {
	return update(ctx, r.db, "knowledge entry", string(id), `DELETE FROM knowledge WHERE id = ?`, string(id))
}

// Search narrows by the indexed columns in SQL, then filters and ranks
// exactly as the in-memory store does, so both backends return the same hits.
// (Full-text ranking via FTS5 can replace the scan later behind this method.)
func (r *sqlKnowledge) Search(ctx context.Context, q domain.KnowledgeQuery) ([]domain.KnowledgeHit, error) {
	query, args := `SELECT data FROM knowledge WHERE 1 = 1`, []any{}
	if q.TaskID != "" {
		query += ` AND task_id = ?`
		args = append(args, string(q.TaskID))
	}
	if q.AgentID != "" {
		query += ` AND agent_id = ?`
		args = append(args, string(q.AgentID))
	}
	entries, err := listJSON[domain.KnowledgeEntry](ctx, r.db, "knowledge", query+` ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	var hits []domain.KnowledgeHit
	for _, e := range entries {
		if score, ok := matchKnowledge(e, q); ok {
			hits = append(hits, domain.KnowledgeHit{Entry: e, Score: score})
		}
	}
	return rankKnowledge(hits, q.Limit), nil
}

// ---------------------------------------------------------------------------
// artifacts
// ---------------------------------------------------------------------------

type sqlArtifacts struct{ db *sql.DB }

var _ ports.ArtifactRepository = (*sqlArtifacts)(nil)

func (r *sqlArtifacts) Create(ctx context.Context, a *domain.Artifact) error {
	data, err := encode(a)
	if err != nil {
		return err
	}
	return insert(ctx, r.db, "artifact", string(a.ID),
		`INSERT INTO artifacts(id, task_id, data) VALUES (?, ?, ?)`, string(a.ID), string(a.TaskID), data)
}

func (r *sqlArtifacts) Get(ctx context.Context, id domain.ArtifactID) (*domain.Artifact, error) {
	return getJSON[domain.Artifact](ctx, r.db, "artifact", string(id), `SELECT data FROM artifacts WHERE id = ?`, string(id))
}

func (r *sqlArtifacts) ListByTask(ctx context.Context, id domain.TaskID) ([]*domain.Artifact, error) {
	return listJSON[domain.Artifact](ctx, r.db, "artifacts",
		`SELECT data FROM artifacts WHERE task_id = ? ORDER BY seq`, string(id))
}

package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// ---------------------------------------------------------------------------
// turns
// ---------------------------------------------------------------------------

type sqlTurns struct{ db dbtx }

var _ ports.TurnRepository = (*sqlTurns)(nil)

const turnColumns = `id, task_id, agent_id, messages, started_at, ended_at, outcome, failure_kind,
	cost_usd, input_tokens, output_tokens, cache_read_tokens`

// turnArgs are a turn's values in turnColumns order, without the id.
func turnArgs(t *domain.Turn) ([]any, error) {
	ids := t.Messages
	if ids == nil {
		ids = []domain.MessageID{}
	}
	msgs, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	ended := ""
	if t.EndedAt != nil {
		ended = formatTime(*t.EndedAt)
	}
	return []any{string(t.TaskID), string(t.AgentID), string(msgs), formatTime(t.StartedAt), ended,
		string(t.Outcome), string(t.FailureKind), t.CostUSD, t.InputTokens, t.OutputTokens, t.CacheReadTokens}, nil
}

func (r *sqlTurns) Create(ctx context.Context, t *domain.Turn) error {
	args, err := turnArgs(t)
	if err != nil {
		return err
	}
	return insert(ctx, r.db, "turn", string(t.ID),
		`INSERT INTO turns(`+turnColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		append([]any{string(t.ID)}, args...)...)
}

func (r *sqlTurns) Update(ctx context.Context, t *domain.Turn) error {
	args, err := turnArgs(t)
	if err != nil {
		return err
	}
	return update(ctx, r.db, "turn", string(t.ID),
		`UPDATE turns SET task_id = ?, agent_id = ?, messages = ?, started_at = ?, ended_at = ?, outcome = ?,
		 failure_kind = ?, cost_usd = ?, input_tokens = ?, output_tokens = ?, cache_read_tokens = ? WHERE id = ?`,
		append(args, string(t.ID))...)
}

func (r *sqlTurns) ListByTask(ctx context.Context, task domain.TaskID) ([]*domain.Turn, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+turnColumns+` FROM turns WHERE task_id = ? ORDER BY seq`, string(task))
	if err != nil {
		return nil, fmt.Errorf("sqlite: list turns: %w", err)
	}
	defer rows.Close()
	out := []*domain.Turn{}
	for rows.Next() {
		var (
			t                                       domain.Turn
			id, tid, agent, msgs, start, end, o, fk string
		)
		if err := rows.Scan(&id, &tid, &agent, &msgs, &start, &end, &o, &fk,
			&t.CostUSD, &t.InputTokens, &t.OutputTokens, &t.CacheReadTokens); err != nil {
			return nil, err
		}
		t.ID, t.TaskID, t.AgentID = domain.TurnID(id), domain.TaskID(tid), domain.AgentID(agent)
		t.Outcome, t.FailureKind = domain.TurnOutcome(o), domain.FailureKind(fk)
		if err := json.Unmarshal([]byte(msgs), &t.Messages); err != nil {
			return nil, fmt.Errorf("sqlite: turn %s messages: %w", id, err)
		}
		if len(t.Messages) == 0 {
			t.Messages = nil
		}
		if t.StartedAt, err = parseTime(start); err != nil {
			return nil, fmt.Errorf("sqlite: turn %s started_at: %w", id, err)
		}
		if end != "" {
			at, err := parseTime(end)
			if err != nil {
				return nil, fmt.Errorf("sqlite: turn %s ended_at: %w", id, err)
			}
			t.EndedAt = &at
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

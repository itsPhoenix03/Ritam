package storage

import (
	"context"
	"database/sql"
	"fmt"

	"ritam/internal/ports"
)

// dbtx is what the repositories run their statements on: the database
// itself, or an open transaction. Building the same repositories on either
// is what lets a transaction see its own writes through the usual methods.
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// storageOn builds the repositories over db, with tx as their transactor.
func storageOn(db dbtx, tx ports.Transactor) *ports.Storage {
	return &ports.Storage{
		Tasks:         &sqlTasks{db},
		Messages:      &sqlMessages{db},
		Events:        &sqlEvents{db},
		Decisions:     &sqlDecisions{db},
		Disagreements: &sqlDisagreements{db},
		Knowledge:     &sqlKnowledge{db},
		Artifacts:     &sqlArtifacts{db},
		Tx:            tx,
	}
}

// sqliteTransactor opens a transaction on the database's one connection.
// Because there is only one, every statement from elsewhere waits until the
// transaction ends: other goroutines never see half of it.
type sqliteTransactor struct{ db *sql.DB }

func (t *sqliteTransactor) WithTx(ctx context.Context, fn func(*ports.Storage) error) (err error) {
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin transaction: %w", err)
	}
	// A panic must still give the connection back: left open, the
	// transaction would hold the only connection and stop every later
	// statement in the process.
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	sp := &sqliteSavepoints{tx: tx}
	if err := fn(storageOn(tx, sp)); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("%w (and the rollback failed: %v)", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit transaction: %w", err)
	}
	return nil
}

// sqliteSavepoints nests a transaction inside an open one with a savepoint,
// so a helper that wants its writes to be all-or-nothing can be called both
// on its own and from inside a larger transaction.
type sqliteSavepoints struct {
	tx    *sql.Tx
	depth int
}

func (t *sqliteSavepoints) WithTx(ctx context.Context, fn func(*ports.Storage) error) error {
	t.depth++
	defer func() { t.depth-- }()
	name := fmt.Sprintf("ritam_sp_%d", t.depth)
	if _, err := t.tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return fmt.Errorf("sqlite: begin nested transaction: %w", err)
	}
	undo := func() error {
		if _, err := t.tx.ExecContext(ctx, "ROLLBACK TO "+name); err != nil {
			return err
		}
		_, err := t.tx.ExecContext(ctx, "RELEASE "+name)
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = undo()
			panic(p)
		}
	}()
	if err := fn(storageOn(t.tx, t)); err != nil {
		if rbErr := undo(); rbErr != nil {
			return fmt.Errorf("%w (and the nested rollback failed: %v)", err, rbErr)
		}
		return err
	}
	if _, err := t.tx.ExecContext(ctx, "RELEASE "+name); err != nil {
		return fmt.Errorf("sqlite: commit nested transaction: %w", err)
	}
	return nil
}

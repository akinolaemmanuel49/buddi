package gormdb

import (
	"context"

	"gorm.io/gorm"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

// Transaction runs fn inside a database transaction. The transaction is stored
// on the context passed to fn, so repositories obtained from ctx-bound code
// automatically write through it. Returning an error from fn rolls the
// transaction back; nil commits it.
//
// When a transaction is already present on ctx, fn joins it instead of opening
// a nested one, and control of commit or rollback stays with the outermost
// caller.
func (d *Database) Transaction(ctx context.Context, fn func(ctx context.Context) error) error {
	if d.db == nil {
		return errNotConnected
	}

	if tx, ok := TxFromContext(ctx); ok {
		return fn(context.WithValue(ctx, txContextKey{}, tx))
	}

	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(context.WithValue(ctx, txContextKey{}, &gormTx{tx: tx}))
	})
}

type txContextKey struct{}

// TxFromContext returns the transaction carried on ctx, if any.
func TxFromContext(ctx context.Context) (persistence.Tx, bool) {
	tx, ok := ctx.Value(txContextKey{}).(persistence.Tx)
	return tx, ok
}

// WithTx returns a context carrying tx, which makes every repository call made
// with it run inside that transaction.
func WithTx(ctx context.Context, tx persistence.Tx) context.Context {
	return context.WithValue(ctx, txContextKey{}, tx)
}

type gormTx struct {
	tx   *gorm.DB
	done bool
}

var _ persistence.Tx = (*gormTx)(nil)

func (t *gormTx) Commit() error {
	if t.done {
		return persistence.ErrTxDone
	}

	t.done = true
	return translate(t.tx.Commit().Error)
}

func (t *gormTx) Rollback() error {
	if t.done {
		return persistence.ErrTxDone
	}

	t.done = true
	return translate(t.tx.Rollback().Error)
}

func (t *gormTx) Conn() *gorm.DB {
	return t.tx
}

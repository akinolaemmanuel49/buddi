package persistence

import (
	"context"
)

// Database is the lifecycle contract for a connection pool.
type Database interface {
	Connect(ctx context.Context) error
	Close() error
	Ping(ctx context.Context) error
	Transactor
}

// Transactor runs work inside a transaction. The transaction is placed on the
// context handed to fn, so any repository resolved within fn participates in it
// without the caller having to thread it through manually. A nested call reuses
// the transaction already on the context instead of opening a new one.
type Transactor interface {
	Transaction(ctx context.Context, fn func(ctx context.Context) error) error
}

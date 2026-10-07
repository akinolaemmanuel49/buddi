package persistence

import (
	"context"

	"gorm.io/gorm"
)

// Tx is an open transaction scope.
type Tx interface {
	Commit() error
	Rollback() error

	// Conn exposes the driver scoped handle for advanced queries.
	Conn() *gorm.DB
}

// Condition is a composable predicate applied to a query.
type Condition func(*gorm.DB) *gorm.DB

// Where matches rows against a SQL predicate, e.g. Where("status = ?", "active").
func Where(query string, args ...any) Condition {
	return func(db *gorm.DB) *gorm.DB {
		return db.Where(query, args...)
	}
}

// WhereNot excludes rows matching a SQL predicate.
func WhereNot(query string, args ...any) Condition {
	return func(db *gorm.DB) *gorm.DB {
		return db.Not(query, args...)
	}
}

// WhereIn matches rows where the column is one of values.
func WhereIn(column string, values ...any) Condition {
	return func(db *gorm.DB) *gorm.DB {
		return db.Where(column+" IN ?", values)
	}
}

// OrderBy sorts the result set, e.g. OrderBy("created_at DESC").
func OrderBy(clause string) Condition {
	return func(db *gorm.DB) *gorm.DB {
		return db.Order(clause)
	}
}

// Preload eagerly loads an association, e.g. Preload("Items").
func Preload(association string, args ...any) Condition {
	return func(db *gorm.DB) *gorm.DB {
		return db.Preload(association, args...)
	}
}

// ListOptions controls pagination and ordering for list queries. Limit and
// Offset are only applied when greater than zero.
type ListOptions struct {
	Limit  int
	Offset int
	Order  string
}

func (o ListOptions) Apply(db *gorm.DB) *gorm.DB {
	if o.Order != "" {
		db = db.Order(o.Order)
	}

	if o.Limit > 0 {
		db = db.Limit(o.Limit)
	}

	if o.Offset > 0 {
		db = db.Offset(o.Offset)
	}

	return db
}

// DefaultLimit is applied to list queries that do not specify their own.
const DefaultLimit = 50

// Repository is the generic persistence contract every aggregate repository
// builds on. T is the entity type, for example domain.User.
//
// Implementations resolve the handle to use per call: the transaction carried
// on the context when present, otherwise the repository's own connection. That
// makes every method transaction aware without extra plumbing.
type Repository[T any] interface {
	// Create inserts entity, populating generated fields such as the primary key.
	Create(ctx context.Context, entity *T) error

	// GetByID returns the entity with the given primary key.
	GetByID(ctx context.Context, id any) (*T, error)

	// First returns the first row matching conds in the model's default order.
	First(ctx context.Context, conds ...Condition) (*T, error)

	// List returns all rows matching conds, paginated by opts.
	List(ctx context.Context, opts ListOptions, conds ...Condition) ([]T, error)

	// Count returns the number of rows matching conds.
	Count(ctx context.Context, conds ...Condition) (int64, error)

	// Exists reports whether any row matches conds.
	Exists(ctx context.Context, conds ...Condition) (bool, error)

	// Update writes every field of entity, matched on its primary key.
	Update(ctx context.Context, entity *T) error

	// Delete removes the row with the given primary key.
	Delete(ctx context.Context, id any) error

	// DeleteWhere removes every row matching conds.
	DeleteWhere(ctx context.Context, conds ...Condition) error

	// WithTx returns a copy of the repository bound to tx. The returned value
	// ignores any transaction on the context it is called with.
	WithTx(tx Tx) Repository[T]

	// Conn returns the resolved handle for the given context, honouring a
	// transaction carried on it.
	Conn(ctx context.Context) *gorm.DB
}

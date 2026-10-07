package gormdb

import (
	"context"
	"reflect"
	"sync"

	"gorm.io/gorm"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

// BaseRepository is the generic repository every concrete repository embeds.
// Embed it by pointer so the concrete type satisfies persistence.Repository:
//
//	type UserRepository struct {
//	    *gormdb.BaseRepository[domain.User]
//	}
//
//	func NewUserRepository(db *gorm.DB) *UserRepository {
//	    return &UserRepository{BaseRepository: gormdb.NewRepository[domain.User](db)}
//	}
//
// It keeps the handle it was built with, and every method resolves the
// transaction on the context first, so repositories work both standalone and
// inside a unit of work without any change in call sites.
type BaseRepository[T any] struct {
	db *gorm.DB
}

var _ persistence.Repository[any] = (*BaseRepository[any])(nil)

// NewRepository returns the generic base repository for entity T bound to db.
func NewRepository[T any](db *gorm.DB) *BaseRepository[T] {
	return &BaseRepository[T]{db: db}
}

// WithTx returns a copy bound to tx, ignoring any transaction on the context.
// The result is the base repository type; concrete repositories that need to
// stay concrete after rebinding build themselves from tx.Conn().
func (r *BaseRepository[T]) WithTx(tx persistence.Tx) persistence.Repository[T] {
	return NewRepository[T](tx.Conn())
}

// Conn resolves the handle for ctx: the transaction carried on the context when
// there is one, otherwise the repository's own handle.
func (r *BaseRepository[T]) Conn(ctx context.Context) *gorm.DB {
	if tx, ok := TxFromContext(ctx); ok {
		return tx.Conn().WithContext(ctx)
	}

	if r.db == nil {
		return nil
	}

	return r.db.WithContext(ctx)
}

// Create inserts entity and writes generated fields back onto it.
func (r *BaseRepository[T]) Create(ctx context.Context, entity *T) error {
	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	return translate(db.Create(entity).Error)
}

// GetByID returns the entity with the given primary key.
func (r *BaseRepository[T]) GetByID(ctx context.Context, id any) (*T, error) {
	var entity T

	db := r.Conn(ctx)
	if db == nil {
		return nil, errNotConnected
	}

	if err := db.Where("id = ?", id).First(&entity).Error; err != nil {
		return nil, translate(err)
	}

	return &entity, nil
}

// First returns the first row matching conds.
func (r *BaseRepository[T]) First(ctx context.Context, conds ...persistence.Condition) (*T, error) {
	var entity T

	db := apply(r.Conn(ctx), conds)
	if db == nil {
		return nil, errNotConnected
	}

	if err := db.First(&entity).Error; err != nil {
		return nil, translate(err)
	}

	return &entity, nil
}

// List returns rows matching conds, paginated by opts. An empty slice is
// returned when nothing matches.
func (r *BaseRepository[T]) List(ctx context.Context, opts persistence.ListOptions, conds ...persistence.Condition) ([]T, error) {
	entities := make([]T, 0)

	db := apply(r.Conn(ctx), conds)
	if db == nil {
		return nil, errNotConnected
	}

	if err := opts.Apply(db).Find(&entities).Error; err != nil {
		return nil, translate(err)
	}

	return entities, nil
}

// Count returns the number of rows matching conds.
func (r *BaseRepository[T]) Count(ctx context.Context, conds ...persistence.Condition) (int64, error) {
	db := r.Conn(ctx)
	if db == nil {
		return 0, errNotConnected
	}

	var total int64

	if err := apply(db.Model(new(T)), conds).Count(&total).Error; err != nil {
		return 0, translate(err)
	}

	return total, nil
}

// Exists reports whether any row matches conds.
func (r *BaseRepository[T]) Exists(ctx context.Context, conds ...persistence.Condition) (bool, error) {
	total, err := r.Count(ctx, conds...)
	if err != nil {
		return false, err
	}

	return total > 0, nil
}

// Update writes every field of entity, matched on its primary key, and reports
// persistence.ErrNotFound when the row does not exist. GORM's Save is
// deliberately avoided because it silently falls back to an upsert.
func (r *BaseRepository[T]) Update(ctx context.Context, entity *T) error {
	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	db = db.Model(entity).Select("*")

	if primary, ok := primaryColumn[T](db); ok {
		db = db.Omit(primary)
	}

	result := db.Updates(entity)
	if err := translate(result.Error); err != nil {
		return err
	}

	if result.RowsAffected == 0 {
		return persistence.ErrNotFound
	}

	return nil
}

// Delete removes the row with the given primary key.
func (r *BaseRepository[T]) Delete(ctx context.Context, id any) error {
	return r.DeleteWhere(ctx, persistence.Where("id = ?", id))
}

// DeleteWhere removes every row matching conds.
func (r *BaseRepository[T]) DeleteWhere(ctx context.Context, conds ...persistence.Condition) error {
	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	return translate(apply(db, conds).Delete(new(T)).Error)
}

func apply(db *gorm.DB, conds []persistence.Condition) *gorm.DB {
	for _, cond := range conds {
		if cond == nil {
			continue
		}

		db = cond(db)
	}

	return db
}

// primaryColumns caches the resolved primary key column per entity type so the
// schema is parsed once instead of on every write.
var primaryColumns sync.Map // reflect.Type -> string

// primaryColumn returns the database column of the primary key of T, parsing the
// schema with the given handle so GORM's schema cache is reused.
func primaryColumn[T any](db *gorm.DB) (string, bool) {
	entityType := reflect.TypeOf((*T)(nil)).Elem()

	if cached, ok := primaryColumns.Load(entityType); ok {
		column, _ := cached.(string)
		return column, column != ""
	}

	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(new(T)); err != nil || stmt.Schema == nil || stmt.Schema.PrioritizedPrimaryField == nil {
		return "", false
	}

	column := stmt.Schema.PrioritizedPrimaryField.DBName
	primaryColumns.Store(entityType, column)

	return column, true
}

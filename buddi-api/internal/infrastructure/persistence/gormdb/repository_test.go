package gormdb

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

type widget struct {
	ID    uint `gorm:"primaryKey"`
	Name  string
	Stock int
}

func (widget) TableName() string { return "widgets" }

func newMockDatabase(t *testing.T) (*Database, sqlmock.Sqlmock) {
	t.Helper()

	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}

	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}

		_ = sqlDB.Close()
	})

	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		DisableAutomaticPing:     true,
		TranslateError:           true,
		SkipDefaultTransaction:   true,
		NowFunc:                  nil,
		DisableNestedTransaction: false,
	})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}

	return &Database{db: gormDB}, mock
}

func TestCreateInsertsAndPopulatesID(t *testing.T) {
	db, mock := newMockDatabase(t)

	mock.ExpectQuery(`INSERT INTO "widgets"`).
		WithArgs("sprocket", 4).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(42))

	repo := NewRepository[widget](db.Conn())

	item := widget{Name: "sprocket", Stock: 4}
	if err := repo.Create(context.Background(), &item); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if item.ID != 42 {
		t.Fatalf("id = %d, want 42", item.ID)
	}
}

func TestGetByIDReturnsEntity(t *testing.T) {
	db, mock := newMockDatabase(t)

	mock.ExpectQuery(`SELECT \* FROM "widgets" WHERE id = `).
		WithArgs(uint(7), 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "stock"}).AddRow(7, "cog", 9))

	repo := NewRepository[widget](db.Conn())

	got, err := repo.GetByID(context.Background(), uint(7))
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	if got.Name != "cog" || got.Stock != 9 {
		t.Fatalf("entity = %+v", got)
	}
}

func TestGetByIDMapsMissingRow(t *testing.T) {
	db, mock := newMockDatabase(t)

	mock.ExpectQuery(`SELECT \* FROM "widgets" WHERE id = `).
		WithArgs(uint(7), 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "stock"}))

	repo := NewRepository[widget](db.Conn())

	_, err := repo.GetByID(context.Background(), uint(7))
	if !persistence.IsNotFound(err) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestListAppliesConditionsAndPagination(t *testing.T) {
	db, mock := newMockDatabase(t)

	mock.ExpectQuery(`SELECT \* FROM "widgets" WHERE stock > \$1 ORDER BY name LIMIT \$2 OFFSET \$3`).
		WithArgs(1, 2, 4).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "stock"}).AddRow(3, "cog", 5))

	repo := NewRepository[widget](db.Conn())

	items, err := repo.List(context.Background(), persistence.ListOptions{
		Limit:  2,
		Offset: 4,
		Order:  "name",
	}, persistence.Where("stock > ?", 1))
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(items) != 1 || items[0].Name != "cog" {
		t.Fatalf("items = %+v", items)
	}
}

func TestCountAndExists(t *testing.T) {
	db, mock := newMockDatabase(t)

	mock.ExpectQuery(`SELECT count\(\*\) FROM "widgets" WHERE name = \$1`).
		WithArgs("cog").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	mock.ExpectQuery(`SELECT count\(\*\) FROM "widgets" WHERE name = \$1`).
		WithArgs("cog").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))

	repo := NewRepository[widget](db.Conn())

	total, err := repo.Count(context.Background(), persistence.Where("name = ?", "cog"))
	if err != nil {
		t.Fatalf("Count: %v", err)
	}

	if total != 3 {
		t.Fatalf("count = %d, want 3", total)
	}

	exists, err := repo.Exists(context.Background(), persistence.Where("name = ?", "cog"))
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}

	if !exists {
		t.Fatal("exists = false, want true")
	}
}

func TestUpdateReportsMissingRow(t *testing.T) {
	db, mock := newMockDatabase(t)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "widgets" SET`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	repo := NewRepository[widget](db.Conn())

	err := db.Transaction(context.Background(), func(ctx context.Context) error {
		return repo.Update(ctx, &widget{ID: 1, Name: "cog"})
	})
	if !persistence.IsNotFound(err) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdateWritesEveryField(t *testing.T) {
	db, mock := newMockDatabase(t)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "widgets" SET "name"=\$1,"stock"=\$2 WHERE "id" = \$3`).
		WithArgs("cog", 5, 1).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := NewRepository[widget](db.Conn())

	err := db.Transaction(context.Background(), func(ctx context.Context) error {
		return repo.Update(ctx, &widget{ID: 1, Name: "cog", Stock: 5})
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestDeleteWhere(t *testing.T) {
	db, mock := newMockDatabase(t)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "widgets" WHERE stock < \$1`).
		WithArgs(3).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	repo := NewRepository[widget](db.Conn())

	if err := db.Transaction(context.Background(), func(ctx context.Context) error {
		return repo.DeleteWhere(ctx, persistence.Where("stock < ?", 3))
	}); err != nil {
		t.Fatalf("DeleteWhere: %v", err)
	}
}

func TestTransactionCommitsOnSuccess(t *testing.T) {
	db, mock := newMockDatabase(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "widgets"`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectCommit()

	repo := NewRepository[widget](db.Conn())

	err := db.Transaction(context.Background(), func(ctx context.Context) error {
		return repo.Create(ctx, &widget{Name: "cog"})
	})
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
}

func TestTransactionRollsBackOnError(t *testing.T) {
	db, mock := newMockDatabase(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "widgets"`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectRollback()

	repo := NewRepository[widget](db.Conn())

	wantErr := errors.New("boom")

	err := db.Transaction(context.Background(), func(ctx context.Context) error {
		if err := repo.Create(ctx, &widget{Name: "cog"}); err != nil {
			t.Fatalf("Create: %v", err)
		}

		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestTransactionVisibleThroughContext(t *testing.T) {
	db, mock := newMockDatabase(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "widgets" WHERE id = `).
		WithArgs(uint(1), 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "stock"}).AddRow(1, "cog", 2))
	mock.ExpectCommit()

	repo := NewRepository[widget](db.Conn())

	var base, scoped *gorm.DB

	err := db.Transaction(context.Background(), func(ctx context.Context) error {
		if _, ok := TxFromContext(ctx); !ok {
			t.Fatal("expected transaction on context")
		}

		base = repo.Conn(ctx)
		scoped = repo.WithTx(mustTx(t, ctx)).Conn(context.Background())

		_, err := repo.GetByID(ctx, uint(1))

		return err
	})
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}

	if base.Statement.ConnPool == repo.Conn(context.Background()).Statement.ConnPool {
		t.Fatal("repository did not resolve the transaction from the context")
	}

	if scoped.Statement.ConnPool != base.Statement.ConnPool {
		t.Fatal("WithTx did not bind to the transaction handle")
	}
}

func TestNestedTransactionReusesOuter(t *testing.T) {
	db, mock := newMockDatabase(t)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "widgets" WHERE stock < \$1`).
		WithArgs(3).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := NewRepository[widget](db.Conn())

	err := db.Transaction(context.Background(), func(ctx context.Context) error {
		return db.Transaction(ctx, func(ctx context.Context) error {
			return repo.DeleteWhere(ctx, persistence.Where("stock < ?", 3))
		})
	})
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
}

func TestDeleteWhereWithoutConditionIsBlocked(t *testing.T) {
	db, _ := newMockDatabase(t)

	repo := NewRepository[widget](db.Conn())

	if err := repo.DeleteWhere(context.Background()); err == nil {
		t.Fatal("expected gorm to reject a delete without conditions")
	}
}

func TestCommitTwiceFails(t *testing.T) {
	db, mock := newMockDatabase(t)

	mock.ExpectBegin()
	mock.ExpectCommit()

	err := db.Transaction(context.Background(), func(ctx context.Context) error {
		tx, ok := TxFromContext(ctx)
		if !ok {
			t.Fatal("expected transaction on context")
		}

		if err := tx.Commit(); err != nil {
			t.Fatalf("first commit: %v", err)
		}

		return tx.Commit()
	})
	if !errors.Is(err, persistence.ErrTxDone) {
		t.Fatalf("err = %v, want ErrTxDone", err)
	}
}

func mustTx(t *testing.T, ctx context.Context) persistence.Tx {
	t.Helper()

	tx, ok := TxFromContext(ctx)
	if !ok {
		t.Fatal("expected transaction on context")
	}

	return tx
}

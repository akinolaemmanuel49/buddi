package gormdb

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

func TestTranslate(t *testing.T) {
	tests := map[string]struct {
		err  error
		want error
	}{
		"nil":              {err: nil, want: nil},
		"record not found": {err: gorm.ErrRecordNotFound, want: persistence.ErrNotFound},
		"duplicated key":   {err: gorm.ErrDuplicatedKey, want: persistence.ErrConflict},
		"pg unique violation": {
			err:  &pgconn.PgError{Code: "23505", ConstraintName: "users_email_key"},
			want: persistence.ErrConflict,
		},
		"pg other error": {
			err:  &pgconn.PgError{Code: "23503"},
			want: nil,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := translate(tt.err)

			if tt.want == nil {
				if got != nil && errors.Is(got, persistence.ErrNotFound) {
					t.Fatalf("unexpected mapping: %v", got)
				}

				return
			}

			if !errors.Is(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

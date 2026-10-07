package gormdb

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

// UserRepository stores identities.
type UserRepository struct {
	*BaseRepository[domain.User]
}

var _ domain.UserRepository = (*UserRepository)(nil)

// NewUserRepository builds the repository. db must be the application's root
// handle; transaction resolution happens per call.
func NewUserRepository(db *gorm.DB) *UserRepository {
	return &UserRepository{BaseRepository: NewRepository[domain.User](db)}
}

// GetByID returns any user without a tenant filter. The auth service uses this
// because it establishes the tenant rather than operating inside one.
func (r *UserRepository) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	return r.First(ctx, persistence.Where("id = ?", id))
}

// GetByEmail looks a user up by address. The column is citext, so the comparison
// is already case insensitive.
func (r *UserRepository) GetByEmail(ctx context.Context, email string) (*domain.User, error) {
	return r.First(ctx, persistence.Where("email = ?", email))
}

// RefreshTokenRepository stores refresh tokens. It is a separate type from
// UserRepository so that a caller holding a token repository cannot reach user
// data through it.
type RefreshTokenRepository struct {
	*BaseRepository[domain.RefreshToken]
}

var _ domain.RefreshTokenRepository = (*RefreshTokenRepository)(nil)

// NewRefreshTokenRepository builds the token repository.
func NewRefreshTokenRepository(db *gorm.DB) *RefreshTokenRepository {
	return &RefreshTokenRepository{BaseRepository: NewRepository[domain.RefreshToken](db)}
}

// GetByHash returns the token matching a hash. The hash is unique, so at most
// one row can match.
func (r *RefreshTokenRepository) GetByHash(ctx context.Context, tokenHash string) (*domain.RefreshToken, error) {
	return r.First(ctx, persistence.Where("token_hash = ?", tokenHash))
}

// RevokeFamily revokes every unrevoked token in a family. This is the response
// to a replayed token: one stolen token invalidates all of its descendants, on
// the assumption that an attacker and the legitimate client now both hold valid
// descendants and we cannot tell them apart.
func (r *RefreshTokenRepository) RevokeFamily(ctx context.Context, familyID uuid.UUID, now time.Time) error {
	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	result := db.Model(&domain.RefreshToken{}).
		Where("family_id = ? AND revoked_at IS NULL", familyID).
		Update("revoked_at", now)

	return translate(result.Error)
}

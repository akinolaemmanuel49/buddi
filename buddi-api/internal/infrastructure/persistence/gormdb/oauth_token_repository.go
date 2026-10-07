package gormdb

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

// OAuthTokenRepository stores provider credentials.
//
// A separate type from UserRepository so that a caller holding a credential store
// cannot reach user data through it: this is the one repository that holds
// long-lived secrets, and the reach it does not have is part of its design.
type OAuthTokenRepository struct {
	*BaseRepository[domain.OAuthToken]
}

var _ domain.OAuthTokenRepository = (*OAuthTokenRepository)(nil)

// NewOAuthTokenRepository builds the repository. db must be the application's root
// handle; transaction resolution happens per call.
func NewOAuthTokenRepository(db *gorm.DB) *OAuthTokenRepository {
	return &OAuthTokenRepository{BaseRepository: NewRepository[domain.OAuthToken](db)}
}

// Get returns the user's credential for one provider.
func (r *OAuthTokenRepository) Get(ctx context.Context, userID uuid.UUID, provider domain.Provider) (*domain.OAuthToken, error) {
	if userID == uuid.Nil {
		return nil, persistence.ErrNotFound
	}

	return r.First(ctx,
		persistence.Where("user_id = ?", userID),
		persistence.Where("provider = ?", string(provider)),
	)
}

// Save writes a credential, replacing any existing row for the same user and
// provider.
//
// The conflict target is the user and provider pair rather than the id. Keying the
// upsert on the id would make a reconnect that mints a fresh id insert a second row
// for a service the user can only connect once, so the credential they think they
// replaced would still be there.
func (r *OAuthTokenRepository) Save(ctx context.Context, token *domain.OAuthToken) error {
	if token == nil {
		return persistence.ErrNotFound
	}

	if token.UserID == uuid.Nil {
		return persistence.ErrNotFound
	}

	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	// All columns: an omitted one keeps the stored value, which for a reconnect means
	// revoking a credential and then writing it again would silently leave it revoked.
	result := db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "provider"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"access_token_ciphertext",
			"refresh_token_ciphertext",
			"scopes",
			"expires_at",
			"revoked_at",
			"updated_at",
		}),
	}).Create(token)

	err := translate(result.Error)
	if err != nil {
		return err
	}

	// A reconnect mints a new id, so after the conflict update the row in the
	// database still carries the old one. Reflecting the stored id back keeps the
	// caller's copy describing the row that exists.
	if result.RowsAffected == 0 {
		return persistence.ErrNotFound
	}

	return r.refreshIdentity(ctx, token)
}

// refreshIdentity re-reads the row's id after an upsert.
func (r *OAuthTokenRepository) refreshIdentity(ctx context.Context, token *domain.OAuthToken) error {
	var stored struct {
		ID uuid.UUID
	}

	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	err := db.Model(&domain.OAuthToken{}).
		Select("id").
		Where("user_id = ? AND provider = ?", token.UserID, string(token.Provider)).
		Take(&stored).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return persistence.ErrNotFound
		}

		return translate(err)
	}

	token.ID = stored.ID

	return nil
}

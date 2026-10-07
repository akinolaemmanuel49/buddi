package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// Provider names a third-party service Buddi can be connected to.
//
// It is stored rather than configured because a user may connect several services,
// and because the provider decides which scopes and endpoints a refresh means. A
// free-form string would let a typo create a second, permanently unrefreshable row
// for a service the user believes is connected.
type Provider string

const (
	ProviderGoogleCalendar Provider = "google_calendar"
)

// AllProviders is the order used to validate a provider.
var AllProviders = []Provider{ProviderGoogleCalendar}

// IsValid reports whether p is a known provider.
func (p Provider) IsValid() bool {
	for _, known := range AllProviders {
		if known == p {
			return true
		}
	}

	return false
}

// joinProviders renders the known providers for a validation message, so the
// accepted values live in one place instead of being restated in the error.
func joinProviders() string {
	names := make([]string, 0, len(AllProviders))

	for _, provider := range AllProviders {
		names = append(names, string(provider))
	}

	return strings.Join(names, ", ")
}

// OAuthToken is one user's stored credential for one provider.
//
// Both tokens are stored encrypted rather than hashed, which is the opposite of how
// the API's own refresh tokens are handled and is deliberate: a hash cannot be
// replayed, so a digest is enough for a credential the server only ever verifies.
// An OAuth refresh token has to be presented to Google to obtain a new access token,
// so the server must be able to recover it, and the protection has to come from the
// key and the storage instead.
//
// There is exactly one row per user and provider, enforced by the unique index. A
// reconnect replaces it.
type OAuthToken struct {
	ID       uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	UserID   uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:idx_oauth_user_provider" json:"user_id"`
	Provider Provider  `gorm:"type:text;not null;uniqueIndex:idx_oauth_user_provider" json:"provider"`

	// AccessTokenCiphertext and RefreshTokenCiphertext hold base64-encoded
	// AES-256-GCM sealed values. Neither is ever marshalled to a client.
	AccessTokenCiphertext  string `gorm:"type:text;not null" json:"-"`
	RefreshTokenCiphertext string `gorm:"type:text;not null" json:"-"`

	// Scopes is the granted scope set, stored so a reconnect can show what the
	// connection can actually do rather than what was requested.
	Scopes string `gorm:"type:text;not null" json:"scopes"`

	// ExpiresAt is when the access token stops working. A refresh is attempted
	// before this, not at it.
	ExpiresAt time.Time `gorm:"type:timestamptz;not null" json:"expires_at"`

	RevokedAt *time.Time `gorm:"type:timestamptz" json:"revoked_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (OAuthToken) TableName() string { return "oauth_tokens" }

// NewOAuthToken validates and stores a credential.
//
// accessCiphertext and refreshCiphertext arrive already encrypted: the domain layer
// never sees a plaintext token, so there is no code path here that could log or
// serialise one.
func NewOAuthToken(
	userID uuid.UUID,
	provider Provider,
	accessCiphertext string,
	refreshCiphertext string,
	scopes string,
	expiresAt time.Time,
	now time.Time,
) (*OAuthToken, error) {
	if userID == uuid.Nil {
		return nil, Invalid("user id is required", nil)
	}

	if !provider.IsValid() {
		return nil, Invalid("provider is not a known provider", map[string]string{
			"provider": "must be one of " + joinProviders(),
		})
	}

	if strings.TrimSpace(accessCiphertext) == "" {
		return nil, Invalid("an encrypted access token is required", nil)
	}

	if strings.TrimSpace(refreshCiphertext) == "" {
		return nil, Invalid("an encrypted refresh token is required", nil)
	}

	return &OAuthToken{
		ID:                     uuid.New(),
		UserID:                 userID,
		Provider:               provider,
		AccessTokenCiphertext:  accessCiphertext,
		RefreshTokenCiphertext: refreshCiphertext,
		Scopes:                 strings.TrimSpace(scopes),
		ExpiresAt:              expiresAt,
		CreatedAt:              now,
		UpdatedAt:              now,
	}, nil
}

// ScopeList returns the granted scopes.
func (t *OAuthToken) ScopeList() []string {
	trimmed := strings.TrimSpace(t.Scopes)
	if trimmed == "" {
		return nil
	}

	parts := strings.Fields(trimmed)
	out := make([]string, 0, len(parts))

	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}

	return out
}

// Connected reports whether the credential may still be used.
func (t *OAuthToken) Connected(now time.Time) bool {
	return t.RevokedAt == nil
}

// Refreshable reports whether the token needs a new access token soon.
//
// The window is a caller-supplied margin rather than a fixed one because only the
// caller knows how long the call it is about to make can take. Refreshing exactly at
// expiry means a request that starts just under the wire is spent.
func (t *OAuthToken) Refreshable(now time.Time, margin time.Duration) bool {
	if t.RevokedAt != nil {
		return false
	}

	if strings.TrimSpace(t.RefreshTokenCiphertext) == "" {
		return false
	}

	return !t.ExpiresAt.After(now.Add(margin))
}

// Revoke marks the credential unusable without deleting the row, so a disconnect is
// auditable and a reconnect can be told apart from one that never happened.
func (t *OAuthToken) Revoke(now time.Time) {
	if t.RevokedAt == nil {
		t.RevokedAt = &now
		t.UpdatedAt = now
	}
}

// ApplyRefresh replaces the credential with the result of a refresh.
//
// Some providers omit the refresh token in their response when it has not changed,
// so an empty refresh is kept rather than treated as a revocation: writing an empty
// one would make the row permanently unrefreshable after the access token expired.
func (t *OAuthToken) ApplyRefresh(
	accessCiphertext string,
	refreshCiphertext string,
	scopes string,
	expiresAt time.Time,
	now time.Time,
) error {
	if strings.TrimSpace(accessCiphertext) == "" {
		return Invalid("an encrypted access token is required", nil)
	}

	t.AccessTokenCiphertext = accessCiphertext

	if strings.TrimSpace(refreshCiphertext) != "" {
		t.RefreshTokenCiphertext = refreshCiphertext
	}

	if trimmed := strings.TrimSpace(scopes); trimmed != "" {
		t.Scopes = trimmed
	}

	t.ExpiresAt = expiresAt
	t.UpdatedAt = now

	return nil
}

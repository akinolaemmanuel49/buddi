package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Password policy. The minimum is deliberately modest because this is a local
// single-user tool; the cost of a weak password is bounded by the fact that
// nothing leaves the machine.
const (
	MinPasswordLength = 8
	// MaxPasswordLength is capped at bcrypt's own limit rather than at a more
	// comfortable number. bcrypt hashes at most 72 bytes of input and silently
	// ignores the rest, so accepting a longer password would mean two different
	// passwords sharing one hash. The limit is checked in bytes as well as in
	// runes for the same reason.
	MaxPasswordLength = 72
	// MaxPasswordBytes is the bcrypt ceiling.
	MaxPasswordBytes = 72
	MaxDisplayName   = 80
)

// User is an authenticated identity. Every other record in the system hangs off
// a user id, which is what makes tenant isolation checkable.
type User struct {
	ID           uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	Email        string    `gorm:"type:citext;not null;unique" json:"email"`
	PasswordHash string    `gorm:"type:text;not null" json:"-"`
	DisplayName  string    `gorm:"type:text;not null" json:"display_name"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (User) TableName() string { return "users" }

// NewUser validates the supplied details and returns a user whose password has
// already been hashed by hashPassword.
func NewUser(email string, passwordHash string, displayName string, now time.Time) (*User, error) {
	normalised, err := normaliseEmail(email)
	if err != nil {
		return nil, err
	}

	name := strings.TrimSpace(displayName)
	if name == "" {
		name = strings.Split(normalised, "@")[0]
	}

	if utf8.RuneCountInString(name) > MaxDisplayName {
		return nil, Invalid("display name is too long", map[string]string{
			"display_name": "must be at most " + strconv.Itoa(MaxDisplayName) + " characters",
		})
	}

	if strings.TrimSpace(passwordHash) == "" {
		return nil, Invalid("a password hash is required", nil)
	}

	return &User{
		ID:           uuid.New(),
		Email:        normalised,
		PasswordHash: passwordHash,
		DisplayName:  name,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// ValidatePassword enforces the password policy. It is separate from
// NewUser because the caller hashes the password first and the policy applies to
// the plaintext.
func ValidatePassword(password string) error {
	switch {
	case utf8.RuneCountInString(password) < MinPasswordLength:
		return Invalid("password is too short", map[string]string{
			"password": "must be at least " + strconv.Itoa(MinPasswordLength) + " characters",
		})
	case utf8.RuneCountInString(password) > MaxPasswordLength:
		return Invalid("password is too long", map[string]string{
			"password": "must be at most " + strconv.Itoa(MaxPasswordLength) + " characters",
		})
	case len(password) > MaxPasswordBytes:
		// Caught separately from the rune count because bcrypt's limit is on
		// bytes, so a short password of multi-byte characters would otherwise
		// pass and then fail, or worse, be silently truncated by the hasher.
		return Invalid("password is too long", map[string]string{
			"password": "must be at most " + strconv.Itoa(MaxPasswordBytes) + " bytes",
		})
	case strings.TrimSpace(password) == "":
		return Invalid("password is only whitespace", map[string]string{
			"password": "must not be entirely whitespace",
		})
	}

	return nil
}

// normaliseEmail trims, lowercases and validates an address. The column is
// citext so uniqueness is case insensitive, but normalising keeps lookups
// predictable.
func normaliseEmail(email string) (string, error) {
	trimmed := strings.TrimSpace(email)

	address, err := mail.ParseAddress(trimmed)
	if err != nil || !strings.Contains(trimmed, "@") {
		return "", Invalid("email address is not valid", map[string]string{
			"email": "must be a valid email address",
		})
	}

	// ParseAddress accepts display name forms such as "A <a@b.com>". Only the
	// bare address is stored.
	normalised := strings.ToLower(address.Address)
	if normalised != strings.ToLower(trimmed) {
		return "", Invalid("email address is not valid", map[string]string{
			"email": "must be a plain address without a display name",
		})
	}

	if len(normalised) > 254 {
		return "", Invalid("email address is too long", map[string]string{
			"email": "must be at most 254 characters",
		})
	}

	return normalised, nil
}

// RefreshToken is a single issued refresh token.
//
// Only the SHA-256 hash of the token is stored. The plaintext exists once, in
// the response to the client, so a database leak cannot be replayed against the
// API.
type RefreshToken struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	UserID     uuid.UUID  `gorm:"type:uuid;not null;index" json:"user_id"`
	TokenHash  string     `gorm:"type:text;not null;unique" json:"-"`
	FamilyID   uuid.UUID  `gorm:"type:uuid;not null;index" json:"family_id"`
	ExpiresAt  time.Time  `gorm:"type:timestamptz;not null" json:"expires_at"`
	RevokedAt  *time.Time `gorm:"type:timestamptz" json:"revoked_at"`
	ReplacedBy *uuid.UUID `gorm:"type:uuid" json:"replaced_by"`
	UserAgent  string     `gorm:"type:text" json:"-"`
	IPAddress  string     `gorm:"type:inet" json:"-"`
	CreatedAt  time.Time  `json:"created_at"`
}

func (RefreshToken) TableName() string { return "refresh_tokens" }

// NewRefreshToken mints an opaque token. Every rotation within one login shares
// a family id so that a replayed token can revoke its siblings.
func NewRefreshToken(userID uuid.UUID, familyID uuid.UUID, ttl time.Duration, now time.Time) (*RefreshToken, *string, error) {
	plaintext, err := randomToken()
	if err != nil {
		return nil, nil, err
	}

	if familyID == uuid.Nil {
		familyID = uuid.New()
	}

	record := &RefreshToken{
		ID:        uuid.New(),
		UserID:    userID,
		TokenHash: HashToken(plaintext),
		FamilyID:  familyID,
		ExpiresAt: now.Add(ttl),
		CreatedAt: now,
	}

	return record, &plaintext, nil
}

// Active reports whether the token may still be exchanged.
func (t *RefreshToken) Active(now time.Time) bool {
	return t.RevokedAt == nil && t.ExpiresAt.After(now)
}

// Revoke marks the token spent. Revoking an already revoked token is a no-op so
// rotation stays safe to retry.
func (t *RefreshToken) Revoke(now time.Time) {
	if t.RevokedAt == nil {
		t.RevokedAt = &now
	}
}

// HashToken is the one way token hashes are produced, so verification and
// storage cannot drift apart.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// randomToken returns a 256 bit random string. crypto/rand failures are fatal
// to the process rather than something to work around.
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(buf), nil
}

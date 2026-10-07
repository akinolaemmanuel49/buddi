// Package auth implements registration, login, token issuance and refresh token
// rotation. It depends only on the domain ports, so it can be tested without a
// database or an HTTP layer.
package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// PasswordHasher turns a plaintext password into something safe to store and
// compares a candidate against a stored hash.
type PasswordHasher interface {
	Hash(plaintext string) (string, error)
	// Compare reports whether plaintext produced hash. It returns
	// domain.ErrUnauthorized on mismatch so callers cannot accidentally treat
	// an error as a successful login.
	Compare(hash string, plaintext string) error
}

// BcryptHasher implements PasswordHasher with bcrypt.
type BcryptHasher struct {
	cost int
}

var _ PasswordHasher = (*BcryptHasher)(nil)

// NewBcryptHasher returns a hasher at the given cost. bcrypt only accepts costs
// between 4 and 31; anything else is a programming error, not a runtime
// condition, so an out of range cost panics here rather than failing later on
// the first registration.
func NewBcryptHasher(cost int) *BcryptHasher {
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		panic("auth: bcrypt cost out of range")
	}

	return &BcryptHasher{cost: cost}
}

func (h *BcryptHasher) Hash(plaintext string) (string, error) {
	hashed, err := bcrypt.GenerateFromPassword([]byte(plaintext), h.cost)
	if err != nil {
		return "", err
	}

	return string(hashed), nil
}

func (h *BcryptHasher) Compare(hash string, plaintext string) error {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext))
	switch {
	case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
		return domain.ErrUnauthorized
	case err != nil:
		// A malformed stored hash is a server side fault, not a wrong password.
		return err
	}

	return nil
}

// dummyHash is compared against when the email is unknown. It exists so login
// takes the same time whether or not the account exists, which stops response
// timing from confirming that an address is registered.
var dummyHash = []byte("$2a$12$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")

// EqualizeTiming performs a throwaway comparison. Callers use it on the paths
// where no real hash is available.
func (h *BcryptHasher) EqualizeTiming(plaintext string) {
	_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(plaintext))
}

// Transactor runs a unit of work inside a transaction. The application layer
// declares its own narrow interface rather than importing the persistence
// package.
type Transactor interface {
	Transaction(ctx context.Context, fn func(ctx context.Context) error) error
}

// TokenMeta records who and where a token was issued. It is diagnostic only.
type TokenMeta struct {
	UserAgent string
	IPAddress string
}

// Session is the credential set returned to a client after a successful
// register, login or refresh.
type Session struct {
	AccessToken string `json:"access_token"`
	// RefreshToken is shown once. Only its hash is stored.
	RefreshToken string       `json:"refresh_token"`
	TokenType    string       `json:"token_type"`
	ExpiresAt    time.Time    `json:"expires_at"`
	User         *domain.User `json:"user"`
}

// tokenType is the scheme clients put in front of the access token.
const tokenType = "Bearer"

// Service holds the dependencies of the authentication flows.
type Service struct {
	users  domain.UserRepository
	tokens domain.RefreshTokenRepository
	hasher PasswordHasher
	issuer TokenIssuer

	accessTTL  time.Duration
	refreshTTL time.Duration

	tx      Transactor
	nowFunc func() time.Time
}

// Options configures a Service.
type Options struct {
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	// Tx is required, because rotating a refresh token writes two rows and they
	// must land together.
	Tx  Transactor
	Now func() time.Time
}

// NewService builds the authentication service.
func NewService(users domain.UserRepository, tokens domain.RefreshTokenRepository, hasher PasswordHasher, issuer TokenIssuer, opts Options) (*Service, error) {
	if users == nil || tokens == nil || hasher == nil || issuer == nil {
		return nil, errors.New("auth: users, tokens, hasher and issuer are required")
	}

	if opts.Tx == nil {
		return nil, errors.New("auth: a transactor is required")
	}

	if opts.AccessTTL <= 0 || opts.RefreshTTL <= 0 {
		return nil, errors.New("auth: access and refresh lifetimes must be positive")
	}

	if opts.RefreshTTL <= opts.AccessTTL {
		return nil, errors.New("auth: refresh lifetime must exceed access lifetime")
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Service{
		users:      users,
		tokens:     tokens,
		hasher:     hasher,
		issuer:     issuer,
		accessTTL:  opts.AccessTTL,
		refreshTTL: opts.RefreshTTL,
		tx:         opts.Tx,
		nowFunc:    now,
	}, nil
}

// RegisterInput carries the details of a new account.
type RegisterInput struct {
	Email       string
	Password    string
	DisplayName string
	Meta        TokenMeta
}

// Register creates an account and immediately signs it in, so a new user does
// not have to submit the form twice.
func (s *Service) Register(ctx context.Context, input RegisterInput) (*Session, error) {
	if err := domain.ValidatePassword(input.Password); err != nil {
		return nil, err
	}

	hash, err := s.hasher.Hash(input.Password)
	if err != nil {
		return nil, err
	}

	now := s.nowFunc()

	user, err := domain.NewUser(input.Email, hash, input.DisplayName, now)
	if err != nil {
		return nil, err
	}

	if err := s.users.Create(ctx, user); err != nil {
		return nil, err
	}

	return s.issue(ctx, user, input.Meta)
}

// Login verifies credentials and starts a token family.
//
// Every failure returns the same error, whether the address is unknown, the
// password is wrong, or both. Together with the throwaway hash comparison in
// the unknown-address path, that keeps the endpoint from revealing which
// addresses are registered.
func (s *Service) Login(ctx context.Context, email string, password string, meta TokenMeta) (*Session, error) {
	user, err := s.users.GetByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.equalize(password)
			return nil, domain.ErrUnauthorized
		}

		return nil, err
	}

	if err := s.hasher.Compare(user.PasswordHash, password); err != nil {
		return nil, domain.ErrUnauthorized
	}

	return s.issue(ctx, user, meta)
}

// Refresh rotates a refresh token.
//
// Presenting a token that was already spent is treated as a compromise: the
// whole family is revoked, which logs out both a thief and the legitimate
// client. The alternative, only revoking the presented token, would let an
// attacker who stole one refresh token keep the legitimate user locked out
// while the attacker's own descendants stayed valid.
func (s *Service) Refresh(ctx context.Context, plaintext string, meta TokenMeta) (*Session, error) {
	if strings.TrimSpace(plaintext) == "" {
		return nil, domain.ErrUnauthorized
	}

	now := s.nowFunc()

	var session *Session

	err := s.tx.Transaction(ctx, func(ctx context.Context) error {
		record, err := s.tokens.GetByHash(ctx, domain.HashToken(plaintext))
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return domain.ErrUnauthorized
			}

			return err
		}

		if record.RevokedAt != nil {
			if err := s.tokens.RevokeFamily(ctx, record.FamilyID, now); err != nil {
				return err
			}

			return domain.ErrTokenReuse
		}

		if !record.ExpiresAt.After(now) {
			return domain.ErrUnauthorized
		}

		next, nextPlaintext, err := domain.NewRefreshToken(record.UserID, record.FamilyID, s.refreshTTL, now)
		if err != nil {
			return err
		}

		next.UserAgent = meta.UserAgent
		next.IPAddress = meta.IPAddress

		// The successor is inserted first because replaced_by is a foreign key:
		// pointing the old row at a successor that does not exist yet fails the
		// constraint. Both writes share the transaction, so rotation is still
		// all-or-nothing.
		// The successor is inserted first because replaced_by is a foreign key:
		// pointing the old row at a successor that does not exist yet fails the
		// constraint. Both writes share the transaction, so rotation is still
		// all-or-nothing. The fake in the tests enforces the same rule.
		if err := s.tokens.Create(ctx, next); err != nil {
			return err
		}

		record.Revoke(now)
		record.ReplacedBy = &next.ID

		if err := s.tokens.Update(ctx, record); err != nil {
			return err
		}

		user, err := s.users.GetByID(ctx, record.UserID)
		if err != nil {
			return err
		}

		access, claims, err := s.issuer.Issue(user.ID, s.accessTTL)
		if err != nil {
			return err
		}

		session = &Session{
			AccessToken:  access,
			RefreshToken: *nextPlaintext,
			TokenType:    tokenType,
			ExpiresAt:    claims.ExpiresAt,
			User:         user,
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return session, nil
}

// Logout revokes a single token. A token that is unknown or already revoked is
// not an error, so logging out twice is harmless.
func (s *Service) Logout(ctx context.Context, plaintext string) error {
	if strings.TrimSpace(plaintext) == "" {
		return nil
	}

	record, err := s.tokens.GetByHash(ctx, domain.HashToken(plaintext))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}

		return err
	}

	if record.RevokedAt != nil {
		return nil
	}

	record.Revoke(s.nowFunc())

	return s.tokens.Update(ctx, record)
}

// Authenticate resolves a verified access token to its user.
func (s *Service) Authenticate(ctx context.Context, accessToken string) (*domain.User, error) {
	claims, err := s.issuer.Verify(accessToken)
	if err != nil {
		return nil, domain.ErrUnauthorized
	}

	user, err := s.users.GetByID(ctx, claims.UserID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// The account was deleted after the token was issued.
			return nil, domain.ErrUnauthorized
		}

		return nil, err
	}

	return user, nil
}

// issue starts a new token family for a user.
func (s *Service) issue(ctx context.Context, user *domain.User, meta TokenMeta) (*Session, error) {
	now := s.nowFunc()

	record, plaintext, err := domain.NewRefreshToken(user.ID, uuid.Nil, s.refreshTTL, now)
	if err != nil {
		return nil, err
	}

	record.UserAgent = meta.UserAgent
	record.IPAddress = meta.IPAddress

	access, claims, err := s.issuer.Issue(user.ID, s.accessTTL)
	if err != nil {
		return nil, err
	}

	if err := s.tokens.Create(ctx, record); err != nil {
		return nil, err
	}

	return &Session{
		AccessToken:  access,
		RefreshToken: *plaintext,
		TokenType:    tokenType,
		ExpiresAt:    claims.ExpiresAt,
		User:         user,
	}, nil
}

// equalize burns roughly the time a real comparison would take.
func (s *Service) equalize(password string) {
	if hasher, ok := s.hasher.(*BcryptHasher); ok {
		hasher.EqualizeTiming(password)

		return
	}

	// A custom hasher has no timing guarantee we can rely on, so do the
	// comparison against a value that cannot match.
	_ = s.hasher.Compare("", password)
}

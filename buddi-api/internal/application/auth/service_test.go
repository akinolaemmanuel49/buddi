package auth_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/auth"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// fakeTransactor runs fn without a database. The transaction semantics that
// matter here, atomicity and rollback, are the repository layer's job and are
// covered by the integration tests; these tests are about the token logic.
type fakeTransactor struct {
	mu    sync.Mutex
	fails bool
	calls int
}

func (t *fakeTransactor) Transaction(ctx context.Context, fn func(ctx context.Context) error) error {
	t.calls++

	if t.fails {
		return errors.New("transaction unavailable")
	}

	return fn(ctx)
}

type fakeUsers struct {
	mu     sync.Mutex
	byID   map[uuid.UUID]*domain.User
	byMail map[string]*domain.User
	email  string
	exists bool
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{byID: map[uuid.UUID]*domain.User{}, byMail: map[string]*domain.User{}}
}

func (f *fakeUsers) Create(_ context.Context, user *domain.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.exists || f.byMail[user.Email] != nil {
		return domain.ErrConflict
	}

	stored := *user
	f.byID[user.ID] = &stored
	f.byMail[user.Email] = &stored

	return nil
}

func (f *fakeUsers) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	user, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}

	copied := *user

	return &copied, nil
}

func (f *fakeUsers) GetByEmail(_ context.Context, email string) (*domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	user, ok := f.byMail[strings.ToLower(strings.TrimSpace(email))]
	if !ok {
		return nil, domain.ErrNotFound
	}

	copied := *user

	return &copied, nil
}

func (f *fakeUsers) Update(_ context.Context, user *domain.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.byID[user.ID]; !ok {
		return domain.ErrNotFound
	}

	stored := *user
	f.byID[user.ID] = &stored

	return nil
}

type fakeTokens struct {
	mu      sync.Mutex
	byHash  map[string]*domain.RefreshToken
	byID    map[uuid.UUID]struct{}
	creates int
}

func newFakeTokens() *fakeTokens {
	return &fakeTokens{byHash: map[string]*domain.RefreshToken{}, byID: map[uuid.UUID]struct{}{}}
}

func (f *fakeTokens) Create(_ context.Context, token *domain.RefreshToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.byHash[token.TokenHash] != nil {
		return domain.ErrConflict
	}

	f.creates++
	stored := *token
	f.byHash[token.TokenHash] = &stored
	f.byID[stored.ID] = struct{}{}

	return nil
}

func (f *fakeTokens) GetByHash(_ context.Context, hash string) (*domain.RefreshToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	token, ok := f.byHash[hash]
	if !ok {
		return nil, domain.ErrNotFound
	}

	copied := *token

	return &copied, nil
}

func (f *fakeTokens) RevokeFamily(_ context.Context, familyID uuid.UUID, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, token := range f.byHash {
		if token.FamilyID == familyID && token.RevokedAt == nil {
			t := *token
			revoked := now
			t.RevokedAt = &revoked
			f.byHash[t.TokenHash] = &t
		}
	}

	return nil
}

func (f *fakeTokens) Update(_ context.Context, token *domain.RefreshToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.byHash[token.TokenHash]; !ok {
		return domain.ErrNotFound
	}

	// replaced_by is a foreign key to refresh_tokens.id, so pointing a row at a
	// successor that has not been inserted yet is a constraint violation, not a
	// detail the fake may ignore. Rotation got this ordering wrong and the unit
	// tests passed anyway; enforcing it here is what makes the fake faithful
	// enough to catch that class of bug.
	if token.ReplacedBy != nil {
		if _, ok := f.byID[*token.ReplacedBy]; !ok {
			return errors.New("fk violation: replaced_by points at a token that does not exist")
		}
	}

	stored := *token
	f.byHash[token.TokenHash] = &stored

	return nil
}

// liveTokenCount is how many tokens in the family are still usable. Reuse tests
// assert on it rather than on individual rows.
func (f *fakeTokens) liveTokenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	count := 0

	for _, token := range f.byHash {
		if token.RevokedAt == nil {
			count++
		}
	}

	return count
}

type fixture struct {
	service *auth.Service
	users   *fakeUsers
	tokens  *fakeTokens
	// clock is a pointer so that the closures handed to the service and the
	// issuer read the current value. Advancing the test's clock has to move the
	// clock the service sees, or expiry tests silently test nothing.
	clock *time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	// A low bcrypt cost keeps the suite fast without changing behaviour: the
	// service never inspects the cost.
	users := newFakeUsers()
	tokens := newFakeTokens()
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	clock := &start
	now := func() time.Time { return *clock }

	// The issuer gets the same clock. Without this the tokens would be minted
	// in March 2026 and verified against the real clock, so every Authenticate
	// would report them as expired.
	issuer := auth.NewJWTIssuer([]byte("test-secret-that-is-long-enough-for-hs256"), "buddi-test")
	issuer.SetClock(now)

	service, err := auth.NewService(
		users,
		tokens,
		auth.NewBcryptHasher(4),
		issuer,
		auth.Options{
			AccessTTL:  15 * time.Minute,
			RefreshTTL: 7 * 24 * time.Hour,
			Tx:         &fakeTransactor{},
			Now:        now,
		},
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	return &fixture{service: service, users: users, tokens: tokens, clock: clock}
}

// advance moves the clock the service and issuer both read.
func (f *fixture) advance(d time.Duration) {
	*f.clock = f.clock.Add(d)
}

func (f *fixture) register(t *testing.T, email string, password string) *auth.Session {
	t.Helper()

	session, err := f.service.Register(context.Background(), auth.RegisterInput{
		Email:       email,
		Password:    password,
		DisplayName: "Test User",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	return session
}

func TestRegisterReturnsUsableSession(t *testing.T) {
	f := newFixture(t)

	session := f.register(t, "user@example.com", "correct horse battery")

	if session.AccessToken == "" || session.RefreshToken == "" {
		t.Fatal("Register must return both tokens")
	}

	if session.TokenType != "Bearer" {
		t.Errorf("TokenType = %q, want Bearer", session.TokenType)
	}

	if want := f.clock.Add(15 * time.Minute); !session.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %s, want %s", session.ExpiresAt, want)
	}

	// The stored record must not be usable to recover the token, and must not
	// carry the plaintext.
	stored, err := f.tokens.GetByHash(context.Background(), domain.HashToken(session.RefreshToken))
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}

	if stored.TokenHash == session.RefreshToken {
		t.Error("the refresh token must be stored hashed, not in plaintext")
	}

	if stored.RevokedAt != nil {
		t.Error("a freshly issued token must not be revoked")
	}

	// The access token must authenticate straight back to the same user.
	user, err := f.service.Authenticate(context.Background(), session.AccessToken)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	if user.ID != session.User.ID {
		t.Errorf("Authenticate returned user %s, want %s", user.ID, session.User.ID)
	}
}

func TestRegisterRejectsDuplicateEmailCaseInsensitively(t *testing.T) {
	f := newFixture(t)
	f.register(t, "user@example.com", "correct horse battery")

	_, err := f.service.Register(context.Background(), auth.RegisterInput{
		Email:    "USER@example.com",
		Password: "another good password",
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("duplicate registration error = %v, want ErrConflict", err)
	}
}

// TestRegisterRejectsWeakPassword pins the policy the domain actually
// enforces: length bounds and "not all whitespace". There is deliberately no
// character class requirement, because this is a local single-user tool and the
// cost of a weak password is bounded by nothing leaving the machine. If the
// policy is tightened, this test is the thing that should fail.
func TestRegisterRejectsWeakPassword(t *testing.T) {
	f := newFixture(t)

	tooShort := []string{"", "short", "1234567"}
	tooLong := []string{
		strings.Repeat("a", domain.MaxPasswordLength+1),
		// Multi-byte characters that are short in runes but long in bytes, which
		// is the case a rune-only limit would let through to bcrypt.
		strings.Repeat("\u00e9", 40),
	}
	whitespaceOnly := []string{"         ", "\t\t\t\t\t\t\t\t"}

	for _, password := range append(append(tooShort, tooLong...), whitespaceOnly...) {
		_, err := f.service.Register(context.Background(), auth.RegisterInput{
			Email:    "user@example.com",
			Password: password,
		})
		if !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("password %q was accepted, want a validation error", truncate(password))
		}
	}

	// The boundaries themselves must be accepted, so the test above cannot pass
	// by rejecting everything.
	boundary := []string{
		strings.Repeat("a", domain.MinPasswordLength),
		strings.Repeat("a", domain.MaxPasswordLength),
		"lowercase and digits",
	}

	for i, password := range boundary {
		if _, err := f.service.Register(context.Background(), auth.RegisterInput{
			Email:    fmt.Sprintf("user%d@example.com", i),
			Password: password,
		}); err != nil {
			t.Errorf("password %q was rejected at the boundary: %v", truncate(password), err)
		}
	}
}

func truncate(s string) string {
	if len(s) > 20 {
		return s[:20] + "..."
	}

	return s
}

func TestRegisterRejectsInvalidEmail(t *testing.T) {
	f := newFixture(t)

	_, err := f.service.Register(context.Background(), auth.RegisterInput{
		Email:    "not-an-email",
		Password: "correct horse battery",
	})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("invalid email error = %v, want ErrInvalid", err)
	}
}

func TestLoginSucceedsAndIsCaseInsensitive(t *testing.T) {
	f := newFixture(t)
	f.register(t, "user@example.com", "correct horse battery")

	session, err := f.service.Login(context.Background(), "  USER@Example.COM ", "correct horse battery", auth.TokenMeta{})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if session.AccessToken == "" {
		t.Error("Login must return an access token")
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	f := newFixture(t)
	f.register(t, "user@example.com", "correct horse battery")

	cases := []struct {
		name     string
		email    string
		password string
	}{
		{name: "unknown address", email: "nobody@example.com", password: "correct horse battery"},
		{name: "wrong password", email: "user@example.com", password: "wrong password entirely"},
		{name: "both wrong", email: "nobody@example.com", password: "wrong password entirely"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.service.Login(context.Background(), tc.email, tc.password, auth.TokenMeta{})
			if !errors.Is(err, domain.ErrUnauthorized) {
				t.Errorf("error = %v, want ErrUnauthorized", err)
			}
		})
	}
}

func TestRefreshRotatesAndInvalidatesTheOldToken(t *testing.T) {
	f := newFixture(t)
	original := f.register(t, "user@example.com", "correct horse battery")

	rotated, err := f.service.Refresh(context.Background(), original.RefreshToken, auth.TokenMeta{UserAgent: "test"})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if rotated.RefreshToken == original.RefreshToken {
		t.Error("Refresh must return a new refresh token")
	}

	if rotated.AccessToken == original.AccessToken {
		t.Error("Refresh must return a new access token")
	}

	// Exactly one token in the family is live after a rotation.
	if got := f.tokens.liveTokenCount(); got != 1 {
		t.Errorf("live tokens after rotation = %d, want 1", got)
	}

	// The successor must be usable.
	if _, err := f.service.Refresh(context.Background(), rotated.RefreshToken, auth.TokenMeta{}); err != nil {
		t.Errorf("the rotated token must be usable, got %v", err)
	}
}

func TestRefreshPreservesTheTokenFamily(t *testing.T) {
	f := newFixture(t)
	original := f.register(t, "user@example.com", "correct horse battery")

	first, err := f.service.Refresh(context.Background(), original.RefreshToken, auth.TokenMeta{})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	before, err := f.tokens.GetByHash(context.Background(), domain.HashToken(original.RefreshToken))
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}

	after, err := f.tokens.GetByHash(context.Background(), domain.HashToken(first.RefreshToken))
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}

	if before.FamilyID != after.FamilyID {
		t.Errorf("rotation changed the family: %s -> %s", before.FamilyID, after.FamilyID)
	}

	// The spent token must point forward at its successor, otherwise a stolen
	// token cannot be traced and the chain of rotations is lost.
	if before.ReplacedBy == nil || *before.ReplacedBy != after.ID {
		t.Error("the spent token must record the token that replaced it")
	}

	if before.RevokedAt == nil {
		t.Error("the spent token must be marked revoked")
	}
}

func TestRefreshRejectsUnknownToken(t *testing.T) {
	f := newFixture(t)

	_, err := f.service.Refresh(context.Background(), "never-issued-token", auth.TokenMeta{})
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("error = %v, want ErrUnauthorized", err)
	}

	_, err = f.service.Refresh(context.Background(), "", auth.TokenMeta{})
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("empty token error = %v, want ErrUnauthorized", err)
	}
}

func TestRefreshRejectsExpiredToken(t *testing.T) {
	f := newFixture(t)
	session := f.register(t, "user@example.com", "correct horse battery")

	// Move the clock past the refresh lifetime.
	f.advance(8 * 24 * time.Hour)

	_, err := f.service.Refresh(context.Background(), session.RefreshToken, auth.TokenMeta{})
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("error = %v, want ErrUnauthorized", err)
	}
}

func TestRefreshDetectsReuseAndRevokesTheFamily(t *testing.T) {
	f := newFixture(t)
	original := f.register(t, "user@example.com", "correct horse battery")

	rotated, err := f.service.Refresh(context.Background(), original.RefreshToken, auth.TokenMeta{})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// The thief replays the token the legitimate client already spent.
	_, err = f.service.Refresh(context.Background(), original.RefreshToken, auth.TokenMeta{})
	if !errors.Is(err, domain.ErrTokenReuse) {
		t.Fatalf("replay error = %v, want ErrTokenReuse", err)
	}

	// The whole family must now be dead, including the descendant the
	// legitimate client is holding.
	if got := f.tokens.liveTokenCount(); got != 0 {
		t.Errorf("live tokens after reuse detection = %d, want 0", got)
	}

	// Presenting an already spent token reports reuse, whether the caller is an
	// attacker replaying the original or the legitimate client holding a
	// descendant. Both are 401s, and neither can be told apart.
	if _, err := f.service.Refresh(context.Background(), rotated.RefreshToken, auth.TokenMeta{}); !isRejected(err) {
		t.Errorf("the descendant must be revoked too, got %v", err)
	}
}

// isRejected reports whether err is one of the two ways a refresh can be turned
// down. Both mean the same thing to a client: this token is no longer usable.
func isRejected(err error) bool {
	return errors.Is(err, domain.ErrUnauthorized) || errors.Is(err, domain.ErrTokenReuse)
}

func TestLogoutRevokesOnlyThePresentedToken(t *testing.T) {
	f := newFixture(t)
	original := f.register(t, "user@example.com", "correct horse battery")

	rotated, err := f.service.Refresh(context.Background(), original.RefreshToken, auth.TokenMeta{})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if err := f.service.Logout(context.Background(), rotated.RefreshToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	if _, err := f.service.Refresh(context.Background(), rotated.RefreshToken, auth.TokenMeta{}); !isRejected(err) {
		t.Errorf("a logged out token must not refresh, got %v", err)
	}
}

func TestLogoutIsIdempotentAndTolerant(t *testing.T) {
	f := newFixture(t)
	session := f.register(t, "user@example.com", "correct horse battery")

	if err := f.service.Logout(context.Background(), session.RefreshToken); err != nil {
		t.Fatalf("first Logout: %v", err)
	}

	if err := f.service.Logout(context.Background(), session.RefreshToken); err != nil {
		t.Errorf("second Logout must be a no-op, got %v", err)
	}

	if err := f.service.Logout(context.Background(), "unknown-token"); err != nil {
		t.Errorf("logging out an unknown token must be a no-op, got %v", err)
	}

	if err := f.service.Logout(context.Background(), ""); err != nil {
		t.Errorf("logging out with no token must be a no-op, got %v", err)
	}
}

func TestAuthenticateRejectsGarbageAndForeignTokens(t *testing.T) {
	f := newFixture(t)

	for _, token := range []string{"", "not.a.jwt", "a.b.c"} {
		if _, err := f.service.Authenticate(context.Background(), token); !errors.Is(err, domain.ErrUnauthorized) {
			t.Errorf("Authenticate(%q) error = %v, want ErrUnauthorized", token, err)
		}
	}

	// A token signed with a different key must not verify.
	foreignIssuer := auth.NewJWTIssuer([]byte("a-completely-different-secret-value"), "buddi-test")
	foreignIssuer.SetClock(func() time.Time { return *f.clock })

	foreignToken, _, err := foreignIssuer.Issue(uuid.New(), time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if _, err := f.service.Authenticate(context.Background(), foreignToken); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("a foreignly signed token must be rejected, got %v", err)
	}
}

func TestAuthenticateRejectsTokenForDeletedUser(t *testing.T) {
	f := newFixture(t)
	session := f.register(t, "user@example.com", "correct horse battery")

	// Simulate the account being deleted after the token was issued.
	f.users.mu.Lock()
	delete(f.users.byID, session.User.ID)
	delete(f.users.byMail, session.User.Email)
	f.users.mu.Unlock()

	if _, err := f.service.Authenticate(context.Background(), session.AccessToken); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("a token for a deleted user must be rejected, got %v", err)
	}
}

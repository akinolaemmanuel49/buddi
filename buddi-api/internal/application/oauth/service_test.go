package oauth_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/oauth"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

var fixedNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// testKey is a real 32 byte key. The tests exercise the production cipher rather than
// a stub: "the token is encrypted" is the property under test, and a stub cannot
// demonstrate it.
func testKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}

	return key
}

func newService(t *testing.T, store oauth.Store, providers ...oauth.Provider) *oauth.Service {
	t.Helper()

	cipher, err := oauth.NewAESCipher(testKey())
	if err != nil {
		t.Fatalf("NewAESCipher: %v", err)
	}

	service, err := oauth.NewService(store, cipher, providers, oauth.Options{
		Clock: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	return service
}

// validGrant is a credential with an hour left, comfortably outside the refresh
// margin, so tests that want the stored token handed straight back get it.
func validGrant() oauth.Grant {
	return oauth.Grant{
		AccessToken:  "ya29.access-token",
		RefreshToken: "1//refresh-token",
		Scopes:       "https://www.googleapis.com/auth/calendar",
		ExpiresAt:    fixedNow.Add(time.Hour),
	}
}

// dueForRefresh is a credential inside the refresh margin, so using it must go
// through the provider first.
func dueForRefresh() oauth.Grant {
	grant := validGrant()
	grant.ExpiresAt = fixedNow.Add(30 * time.Second)

	return grant
}

// fakeProvider stands in for Google. It is the reason no real credential has to exist
// for this package to be tested.
type fakeProvider struct {
	name domain.Provider

	// result is what the next Refresh returns.
	result *oauth.RefreshedToken
	err    error

	// calls records what refresh tokens it was handed.
	calls []string
}

func (p *fakeProvider) Name() domain.Provider { return p.name }

func (p *fakeProvider) Refresh(_ context.Context, refreshToken string) (*oauth.RefreshedToken, error) {
	p.calls = append(p.calls, refreshToken)

	if p.err != nil {
		return nil, p.err
	}

	return p.result, nil
}

// The stored row must not contain anything usable. This is the single most important
// assertion in the package: a refresh token in plaintext is a live credential for a
// real account.
func TestConnectEncryptsTheStoredTokens(t *testing.T) {
	store := newFakeStore()
	service := newService(t, store)

	grant := validGrant()

	if err := service.Connect(t.Context(), uuid.New(), domain.ProviderGoogleCalendar, grant); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	stored := store.only()

	if strings.Contains(stored.AccessTokenCiphertext, grant.AccessToken) {
		t.Error("the stored access token contains the plaintext")
	}

	if strings.Contains(stored.RefreshTokenCiphertext, grant.RefreshToken) {
		t.Error("the stored refresh token contains the plaintext")
	}

	// The ciphertext has to be unsealable, not merely different: a one-way hash would
	// pass the check above and make the credential unusable.
	cipher, err := oauth.NewAESCipher(testKey())
	if err != nil {
		t.Fatalf("NewAESCipher: %v", err)
	}

	access, err := cipher.Open(stored.AccessTokenCiphertext)
	if err != nil {
		t.Fatalf("Open access token: %v", err)
	}

	if string(access) != grant.AccessToken {
		t.Errorf("access token = %q, want %q", access, grant.AccessToken)
	}

	refresh, err := cipher.Open(stored.RefreshTokenCiphertext)
	if err != nil {
		t.Fatalf("Open refresh token: %v", err)
	}

	if string(refresh) != grant.RefreshToken {
		t.Errorf("refresh token = %q, want %q", refresh, grant.RefreshToken)
	}
}

// A wrong key must fail rather than produce a token. GCM's tag is what makes that
// true, and it is the difference between "this row was tampered with" and "this row
// now holds a credential an attacker chose".
func TestCipherRefusesATamperedOrForeignValue(t *testing.T) {
	cipher, err := oauth.NewAESCipher(testKey())
	if err != nil {
		t.Fatalf("NewAESCipher: %v", err)
	}

	sealed, err := cipher.Seal([]byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// A different key must not open it.
	otherKey := make([]byte, 32)
	copy(otherKey, testKey())
	otherKey[0]++

	other, err := oauth.NewAESCipher(otherKey)
	if err != nil {
		t.Fatalf("NewAESCipher: %v", err)
	}

	if _, err := other.Open(sealed); err == nil {
		t.Error("a ciphertext opened under a different key")
	}

	// Flipping one bit of the ciphertext must also fail.
	raw := []byte(sealed)
	if raw[len(raw)-1] == 'A' {
		raw[len(raw)-1] = 'B'
	} else {
		raw[len(raw)-1] = 'A'
	}

	if _, err := cipher.Open(string(raw)); err == nil {
		t.Error("a tampered ciphertext opened successfully")
	}
}

func TestNewAESCipherRefusesAKeyOfTheWrongSize(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33} {
		if _, err := oauth.NewAESCipher(make([]byte, size)); err == nil {
			t.Errorf("NewAESCipher accepted a %d byte key", size)
		}
	}
}

func TestAccessTokenReturnsTheStoredCredential(t *testing.T) {
	store := newFakeStore()
	service := newService(t, store)

	user := uuid.New()
	grant := validGrant()

	if err := service.Connect(t.Context(), user, domain.ProviderGoogleCalendar, grant); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	token, err := service.AccessToken(t.Context(), user, domain.ProviderGoogleCalendar)
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	if token != grant.AccessToken {
		t.Errorf("access token = %q, want %q", token, grant.AccessToken)
	}
}

// A token that is about to expire has to be refreshed before it is handed out, or
// the call that follows it fails for a reason the user cannot act on.
func TestAccessTokenRefreshesBeforeExpiry(t *testing.T) {
	store := newFakeStore()
	provider := &fakeProvider{
		name: domain.ProviderGoogleCalendar,
		result: &oauth.RefreshedToken{
			AccessToken: "ya29.refreshed",
			Scopes:      validGrant().Scopes,
			ExpiresAt:   fixedNow.Add(time.Hour),
		},
	}

	service := newService(t, store, provider)

	user := uuid.New()

	if err := service.Connect(t.Context(), user, domain.ProviderGoogleCalendar, dueForRefresh()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	token, err := service.AccessToken(t.Context(), user, domain.ProviderGoogleCalendar)
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	if token != "ya29.refreshed" {
		t.Errorf("access token = %q, want the refreshed one", token)
	}

	// The refresh has to be given the real refresh token, decrypted.
	if len(provider.calls) != 1 || provider.calls[0] != validGrant().RefreshToken {
		t.Errorf("refresh calls = %v, want the stored refresh token", provider.calls)
	}
}

// A provider that does not rotate its refresh token returns nothing for one. Writing
// that empty value would make the credential permanently unrefreshable, so the
// stored one has to stand.
func TestRefreshKeepsTheExistingRefreshTokenWhenTheProviderOmitsIt(t *testing.T) {
	store := newFakeStore()
	provider := &fakeProvider{
		name: domain.ProviderGoogleCalendar,
		result: &oauth.RefreshedToken{
			AccessToken: "ya29.refreshed",
			ExpiresAt:   fixedNow.Add(time.Hour),
		},
	}

	service := newService(t, store, provider)

	user := uuid.New()
	grant := validGrant()

	if err := service.Connect(t.Context(), user, domain.ProviderGoogleCalendar, grant); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if _, err := service.AccessToken(t.Context(), user, domain.ProviderGoogleCalendar); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	stored := store.only()

	cipher, err := oauth.NewAESCipher(testKey())
	if err != nil {
		t.Fatalf("NewAESCipher: %v", err)
	}

	refresh, err := cipher.Open(stored.RefreshTokenCiphertext)
	if err != nil {
		t.Fatalf("Open refresh token: %v", err)
	}

	if string(refresh) != grant.RefreshToken {
		t.Errorf("refresh token = %q, want the original to be kept", refresh)
	}
}

func TestAccessTokenRefreshesARotatedRefreshToken(t *testing.T) {
	store := newFakeStore()
	provider := &fakeProvider{
		name: domain.ProviderGoogleCalendar,
		result: &oauth.RefreshedToken{
			AccessToken:  "ya29.refreshed",
			RefreshToken: "1//rotated",
			ExpiresAt:    fixedNow.Add(time.Hour),
		},
	}

	service := newService(t, store, provider)

	user := uuid.New()

	if err := service.Connect(t.Context(), user, domain.ProviderGoogleCalendar, dueForRefresh()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if _, err := service.AccessToken(t.Context(), user, domain.ProviderGoogleCalendar); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	cipher, err := oauth.NewAESCipher(testKey())
	if err != nil {
		t.Fatalf("NewAESCipher: %v", err)
	}

	refresh, err := cipher.Open(store.only().RefreshTokenCiphertext)
	if err != nil {
		t.Fatalf("Open refresh token: %v", err)
	}

	if string(refresh) != "1//rotated" {
		t.Errorf("refresh token = %q, want the rotated one", refresh)
	}
}

// A failed refresh must not leave the credential unusable: the access token is still
// stored, and the next call can try again.
func TestAFailedRefreshLeavesTheStoredCredentialAlone(t *testing.T) {
	store := newFakeStore()
	provider := &fakeProvider{name: domain.ProviderGoogleCalendar, err: errors.New("network unreachable")}

	service := newService(t, store, provider)

	user := uuid.New()

	if err := service.Connect(t.Context(), user, domain.ProviderGoogleCalendar, dueForRefresh()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if _, err := service.AccessToken(t.Context(), user, domain.ProviderGoogleCalendar); err == nil {
		t.Fatal("AccessToken succeeded with a failing provider")
	}

	stored := store.only()
	if stored.AccessTokenCiphertext == "" || stored.RefreshTokenCiphertext == "" {
		t.Error("the failed refresh cleared the stored credential")
	}
}

// A provider that returns no access token must not be treated as a success that
// happens to store nothing.
func TestARefreshReturningNothingIsAnError(t *testing.T) {
	store := newFakeStore()
	provider := &fakeProvider{name: domain.ProviderGoogleCalendar, result: &oauth.RefreshedToken{}}

	service := newService(t, store, provider)

	user := uuid.New()

	if err := service.Connect(t.Context(), user, domain.ProviderGoogleCalendar, dueForRefresh()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if _, err := service.AccessToken(t.Context(), user, domain.ProviderGoogleCalendar); err == nil {
		t.Fatal("AccessToken succeeded when the refresh returned no access token")
	}
}

func TestADisconnectedCredentialIsRefused(t *testing.T) {
	store := newFakeStore()
	service := newService(t, store)

	user := uuid.New()

	if err := service.Connect(t.Context(), user, domain.ProviderGoogleCalendar, dueForRefresh()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if err := service.Disconnect(t.Context(), user, domain.ProviderGoogleCalendar); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	if _, err := service.AccessToken(t.Context(), user, domain.ProviderGoogleCalendar); !errors.Is(err, oauth.ErrRevoked) {
		t.Errorf("error = %v, want ErrRevoked", err)
	}

	if _, err := service.Status(t.Context(), user, domain.ProviderGoogleCalendar); !errors.Is(err, oauth.ErrRevoked) {
		t.Errorf("status error = %v, want ErrRevoked", err)
	}
}

// One user's credential must be invisible to another, including for the same
// provider. The provider is not a tenant.
func TestCredentialsAreScopedToTheirUser(t *testing.T) {
	store := newFakeStore()
	service := newService(t, store)

	mine := uuid.New()
	theirs := uuid.New()

	if err := service.Connect(t.Context(), mine, domain.ProviderGoogleCalendar, validGrant()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if _, err := service.AccessToken(t.Context(), theirs, domain.ProviderGoogleCalendar); !errors.Is(err, oauth.ErrNotConnected) {
		t.Errorf("another user read the credential: err = %v, want ErrNotConnected", err)
	}
}

func TestStatusOfAnUnconnectedProvider(t *testing.T) {
	service := newService(t, newFakeStore())

	status, err := service.Status(t.Context(), uuid.New(), domain.ProviderGoogleCalendar)
	if !errors.Is(err, oauth.ErrNotConnected) {
		t.Errorf("error = %v, want ErrNotConnected", err)
	}

	if status.Connected {
		t.Error("Connected = true for a provider that was never connected")
	}
}

// Reconnecting has to replace the credential rather than leave the old one usable,
// and must not be distinguishable from having been revoked first.
func TestReconnectReplacesTheStoredCredential(t *testing.T) {
	store := newFakeStore()
	service := newService(t, store)

	user := uuid.New()

	if err := service.Connect(t.Context(), user, domain.ProviderGoogleCalendar, dueForRefresh()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	second := validGrant()
	second.AccessToken = "ya29.second"

	if err := service.Connect(t.Context(), user, domain.ProviderGoogleCalendar, second); err != nil {
		t.Fatalf("second Connect: %v", err)
	}

	if store.count() != 1 {
		t.Errorf("rows = %d, want 1", store.count())
	}

	token, err := service.AccessToken(t.Context(), user, domain.ProviderGoogleCalendar)
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	if token != second.AccessToken {
		t.Errorf("access token = %q, want the reconnected one", token)
	}
}

func TestNewServiceValidatesItsDependencies(t *testing.T) {
	cipher, err := oauth.NewAESCipher(testKey())
	if err != nil {
		t.Fatalf("NewAESCipher: %v", err)
	}

	store := newFakeStore()
	provider := &fakeProvider{name: domain.ProviderGoogleCalendar}

	if _, err := oauth.NewService(nil, cipher, nil, oauth.Options{}); err == nil {
		t.Error("NewService accepted a nil store")
	}

	if _, err := oauth.NewService(store, nil, nil, oauth.Options{}); err == nil {
		t.Error("NewService accepted a nil cipher")
	}

	if _, err := oauth.NewService(store, cipher, []oauth.Provider{nil}, oauth.Options{}); err == nil {
		t.Error("NewService accepted a nil provider")
	}

	if _, err := oauth.NewService(store, cipher, []oauth.Provider{
		provider,
		&fakeProvider{name: domain.ProviderGoogleCalendar},
	}, oauth.Options{}); err == nil {
		t.Error("NewService accepted two providers with the same name")
	}

	if _, err := oauth.NewService(store, cipher, []oauth.Provider{
		&fakeProvider{name: domain.Provider("not_a_provider")},
	}, oauth.Options{}); err == nil {
		t.Error("NewService accepted an unknown provider name")
	}
}

func TestConnectValidatesTheGrant(t *testing.T) {
	service := newService(t, newFakeStore())

	missingAccess := validGrant()
	missingAccess.AccessToken = ""

	missingRefresh := validGrant()
	missingRefresh.RefreshToken = ""

	if err := service.Connect(t.Context(), uuid.New(), domain.ProviderGoogleCalendar, missingAccess); err == nil {
		t.Error("Connect accepted a grant with no access token")
	}

	if err := service.Connect(t.Context(), uuid.New(), domain.ProviderGoogleCalendar, missingRefresh); err == nil {
		t.Error("Connect accepted a grant with no refresh token")
	}
}

// fakeStore keeps one credential per user and provider, which is the constraint the
// unique index enforces in the database.
type fakeStore struct {
	rows map[string]*domain.OAuthToken
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: make(map[string]*domain.OAuthToken)}
}

func storeKey(userID uuid.UUID, provider domain.Provider) string {
	return fmt.Sprintf("%s/%s", userID, provider)
}

func (s *fakeStore) Get(_ context.Context, userID uuid.UUID, provider domain.Provider) (*domain.OAuthToken, error) {
	record, ok := s.rows[storeKey(userID, provider)]
	if !ok {
		return nil, domain.ErrNotFound
	}

	// A copy, so a caller mutating what it read cannot change what is stored without
	// going through Save.
	copied := *record

	return &copied, nil
}

func (s *fakeStore) Save(_ context.Context, token *domain.OAuthToken) error {
	copied := *token
	s.rows[storeKey(token.UserID, token.Provider)] = &copied

	return nil
}

func (s *fakeStore) only() *domain.OAuthToken {
	for _, record := range s.rows {
		return record
	}

	return nil
}

func (s *fakeStore) count() int { return len(s.rows) }

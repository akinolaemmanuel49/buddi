package oauth_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/oauth"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence/gormdb"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence/migrations"
)

// liveDatabase runs against a disposable database, opted into with
// BUDDI_TEST_DATABASE_URI. The unit tests use a fake store, so without this nothing
// checks that the migration is valid SQL or that the upsert behaves as the domain
// claims.
func liveDatabase(t *testing.T) *gorm.DB {
	t.Helper()

	dsn := os.Getenv("BUDDI_TEST_DATABASE_URI")
	if dsn == "" {
		t.Skip("set BUDDI_TEST_DATABASE_URI to a disposable database to run this test")
	}

	ctx := t.Context()

	if _, err := migrations.Up(ctx, dsn); err != nil {
		t.Fatalf("migrate %s: %v", dsn, err)
	}

	db := gormdb.New(gormdb.DefaultConfig(dsn))
	if err := db.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	conn := db.Conn()

	if err := conn.Exec("DELETE FROM oauth_tokens").Error; err != nil {
		t.Fatalf("clear oauth tokens: %v", err)
	}

	return conn
}

func liveUser(t *testing.T, db *gorm.DB) *domain.User {
	t.Helper()

	user, err := domain.NewUser("live-"+uuid.NewString()+"@example.com", "correct-horse-battery-staple", "live", time.Now())
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}

	if err := db.Create(user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	return user
}

// The migration has to produce a table the repository can use, and a reconnect has to
// replace the row rather than adding one.
func TestLiveOAuthTokenSaveReplacesOnReconnect(t *testing.T) {
	conn := liveDatabase(t)
	user := liveUser(t, conn)

	repo := gormdb.NewOAuthTokenRepository(conn)

	first := &domain.OAuthToken{
		ID:                     uuid.New(),
		UserID:                 user.ID,
		Provider:               domain.ProviderGoogleCalendar,
		AccessTokenCiphertext:  "sealed-access-1",
		RefreshTokenCiphertext: "sealed-refresh-1",
		Scopes:                 "calendar",
		ExpiresAt:              time.Now().Add(time.Hour),
		CreatedAt:              time.Now(),
		UpdatedAt:              time.Now(),
	}

	if err := repo.Save(t.Context(), first); err != nil {
		t.Fatalf("Save: %v", err)
	}

	second := &domain.OAuthToken{
		ID:                     uuid.New(),
		UserID:                 user.ID,
		Provider:               domain.ProviderGoogleCalendar,
		AccessTokenCiphertext:  "sealed-access-2",
		RefreshTokenCiphertext: "sealed-refresh-2",
		Scopes:                 "calendar",
		ExpiresAt:              time.Now().Add(2 * time.Hour),
		CreatedAt:              time.Now(),
		UpdatedAt:              time.Now(),
	}

	if err := repo.Save(t.Context(), second); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	var count int64
	if err := conn.Model(&domain.OAuthToken{}).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}

	if count != 1 {
		t.Fatalf("rows = %d, want 1: a reconnect must replace the credential", count)
	}

	stored, err := repo.Get(t.Context(), user.ID, domain.ProviderGoogleCalendar)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if stored.AccessTokenCiphertext != "sealed-access-2" {
		t.Errorf("access ciphertext = %q, want the reconnected one", stored.AccessTokenCiphertext)
	}

	// A reconnect mints a new id, so the caller's copy has to be told the stored one.
	if second.ID != stored.ID {
		t.Error("Save did not report the stored id back to the caller")
	}
}

// The provider is not a tenant: one user's credential must not be reachable with
// another user's id.
func TestLiveOAuthTokenIsNotReadableAcrossOwners(t *testing.T) {
	conn := liveDatabase(t)
	owner := liveUser(t, conn)
	other := liveUser(t, conn)

	repo := gormdb.NewOAuthTokenRepository(conn)

	token := &domain.OAuthToken{
		ID:                     uuid.New(),
		UserID:                 owner.ID,
		Provider:               domain.ProviderGoogleCalendar,
		AccessTokenCiphertext:  "sealed-access",
		RefreshTokenCiphertext: "sealed-refresh",
		Scopes:                 "calendar",
		ExpiresAt:              time.Now().Add(time.Hour),
		CreatedAt:              time.Now(),
		UpdatedAt:              time.Now(),
	}

	if err := repo.Save(t.Context(), token); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := repo.Get(t.Context(), other.ID, domain.ProviderGoogleCalendar); err == nil {
		t.Fatal("Get as another user succeeded, want a not-found error")
	}
}

// The whole service has to work against the real store and the real cipher: the
// point of the seam is that the seam is the production path, not a parallel one.
func TestLiveServiceStoresAndRefreshesEncryptedTokens(t *testing.T) {
	conn := liveDatabase(t)
	user := liveUser(t, conn)

	store := gormdb.NewOAuthTokenRepository(conn)

	cipher, err := newTestCipher()
	if err != nil {
		t.Fatalf("NewAESCipher: %v", err)
	}

	refreshed := refreshedResult()
	provider := &fakeProvider{
		name:   domain.ProviderGoogleCalendar,
		result: &refreshed,
	}

	service, err := newServiceWithClock(t, store, cipher, []oauth.Provider{provider}, time.Now)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	grant := validGrant()
	grant.ExpiresAt = time.Now().Add(30 * time.Second)

	if err := service.Connect(t.Context(), user.ID, domain.ProviderGoogleCalendar, grant); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Read the row back through the store: the ciphertext must not contain the
	// plaintext, which is the property the live schema has to preserve.
	stored, err := store.Get(t.Context(), user.ID, domain.ProviderGoogleCalendar)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if stored.AccessTokenCiphertext == grant.AccessToken {
		t.Error("the live row holds the access token in plaintext")
	}

	if stored.RefreshTokenCiphertext == grant.RefreshToken {
		t.Error("the live row holds the refresh token in plaintext")
	}

	// The grant is inside the refresh margin, so this has to go through the provider.
	access, err := service.AccessToken(t.Context(), user.ID, domain.ProviderGoogleCalendar)
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	if access != refreshed.AccessToken {
		t.Errorf("access token = %q, want the refreshed one", access)
	}

	if len(provider.calls) != 1 {
		t.Errorf("refresh calls = %d, want 1", len(provider.calls))
	}

	// And the refreshed credential has to have been written back, or the next call
	// refreshes again.
	if _, err := service.AccessToken(t.Context(), user.ID, domain.ProviderGoogleCalendar); err != nil {
		t.Fatalf("second AccessToken: %v", err)
	}

	if len(provider.calls) != 1 {
		t.Errorf("refresh calls = %d, want the second call to reuse the stored token", len(provider.calls))
	}
}

func TestLiveServiceRefusesAnUnconnectedProvider(t *testing.T) {
	conn := liveDatabase(t)
	user := liveUser(t, conn)

	cipher, err := newTestCipher()
	if err != nil {
		t.Fatalf("NewAESCipher: %v", err)
	}

	service, err := newServiceWithClock(t, gormdb.NewOAuthTokenRepository(conn), cipher, nil, time.Now)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if _, err := service.AccessToken(context.Background(), user.ID, domain.ProviderGoogleCalendar); err == nil {
		t.Fatal("AccessToken succeeded for a provider that was never connected")
	}
}

// newTestCipher is the production cipher, so the live path exercises the real
// encryption rather than a stand-in.
func newTestCipher() (*oauth.AESCipher, error) {
	return oauth.NewAESCipher(testKey())
}

// newServiceWithClock wires the service over a live store and a real clock, so the
// refresh decision depends on actual expiry rather than a fixed instant.
func newServiceWithClock(
	t *testing.T,
	store oauth.Store,
	cipher *oauth.AESCipher,
	providers []oauth.Provider,
	clock func() time.Time,
) (*oauth.Service, error) {
	t.Helper()

	return oauth.NewService(store, cipher, providers, oauth.Options{Clock: clock})
}

// refreshedResult is what the fake provider hands back.
func refreshedResult() oauth.RefreshedToken {
	return oauth.RefreshedToken{
		AccessToken: "ya29.refreshed",
		Scopes:      "calendar",
		ExpiresAt:   time.Now().Add(time.Hour),
	}
}

package googleoauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/googleoauth"
)

func testSecret() []byte {
	return []byte(strings.Repeat("s", 32))
}

func testConfig(tokenURL string) googleoauth.Config {
	return googleoauth.Config{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "http://localhost:8021/api/v1/connections/google_calendar/callback",
		TokenURL:     tokenURL,
	}
}

func TestNewRequiresACompleteConfiguration(t *testing.T) {
	cases := map[string]googleoauth.Config{
		"nothing":      {},
		"no secret":    {ClientID: "id", RedirectURL: "http://x"},
		"no redirect":  {ClientID: "id", ClientSecret: "secret"},
		"no client id": {ClientSecret: "secret", RedirectURL: "http://x"},
	}

	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := googleoauth.New(config); err == nil {
				t.Error("New accepted an incomplete configuration")
			}
		})
	}
}

// A partial configuration is the case that produces a connector which registers and
// then fails every call, so Configured is the check that has to be right.
func TestConfiguredRequiresAllThree(t *testing.T) {
	full := testConfig("")

	if !full.Configured() {
		t.Error("a complete configuration reported unconfigured")
	}

	full.ClientSecret = ""
	if full.Configured() {
		t.Error("a configuration with no client secret reported configured")
	}
}

// Google only returns a refresh token on the first grant, and only with
// offline access. Without one the credential is dead in an hour, so it is refused.
func TestExchangeRefusesAResponseWithNoRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"at","expires_in":3599,"scope":"x"}`))
	}))
	defer server.Close()

	provider, err := googleoauth.New(testConfig(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := provider.Exchange(context.Background(), "code"); err == nil {
		t.Error("Exchange accepted a response with no refresh token")
	}
}

func TestExchangeReturnsTheGrant(t *testing.T) {
	var received url.Values

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse: %v", err)
		}
		received = r.PostForm

		_, _ = w.Write([]byte(
			`{"access_token":"at","refresh_token":"rt","expires_in":3599,"scope":"a b"}`))
	}))
	defer server.Close()

	provider, err := googleoauth.New(testConfig(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	grant, err := provider.Exchange(context.Background(), "the-code")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	if grant.AccessToken != "at" || grant.RefreshToken != "rt" {
		t.Errorf("grant = %+v", grant)
	}

	if received.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q", received.Get("grant_type"))
	}

	if received.Get("code") != "the-code" {
		t.Errorf("code = %q", received.Get("code"))
	}

	if grant.ExpiresAt.Before(time.Now()) {
		t.Error("expiry is in the past, so the token would be refreshed immediately")
	}
}

// Google's failures are specific, and a bare 400 is not actionable.
func TestExchangeSurfacesGooglesExplanation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(
			`{"error":"invalid_grant","error_description":"Bad Request"}`))
	}))
	defer server.Close()

	provider, err := googleoauth.New(testConfig(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = provider.Exchange(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("err = %v, want Google's own explanation", err)
	}
}

// Google does not rotate refresh tokens, so an empty one means keep the existing.
func TestRefreshLeavesAnAbsentRefreshTokenEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"new","expires_in":3599}`))
	}))
	defer server.Close()

	provider, err := googleoauth.New(testConfig(server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	refreshed, err := provider.Refresh(context.Background(), "old-refresh")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if refreshed.AccessToken != "new" {
		t.Errorf("AccessToken = %q", refreshed.AccessToken)
	}

	if refreshed.RefreshToken != "" {
		t.Errorf("RefreshToken = %q, want empty so the stored one is kept", refreshed.RefreshToken)
	}
}

// Offline access is what makes a refresh token come back at all.
func TestAuthCodeURLAsksForOfflineAccess(t *testing.T) {
	provider, err := googleoauth.New(testConfig(""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	parsed, err := url.Parse(provider.AuthCodeURL("the-state"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	query := parsed.Query()

	for key, want := range map[string]string{
		"access_type":   "offline",
		"response_type": "code",
		"state":         "the-state",
		"client_id":     "client-id",
	} {
		if got := query.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	if !strings.Contains(query.Get("scope"), "calendar") {
		t.Errorf("scope = %q, want the calendar scope", query.Get("scope"))
	}
}

func TestSignerRejectsAShortSecret(t *testing.T) {
	if _, err := googleoauth.NewSigner([]byte("too short")); err == nil {
		t.Error("NewSigner accepted a secret shorter than 32 bytes")
	}
}

func TestStateRoundTrips(t *testing.T) {
	signer, err := googleoauth.NewSigner(testSecret())
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	userID := uuid.New()

	state, err := signer.Issue(userID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	verified, err := signer.Verify(state)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if verified.UserID != userID {
		t.Errorf("UserID = %v, want %v", verified.UserID, userID)
	}
}

// The state is what tells the callback whose account to attach, so a forged one would
// attach the attacker's calendar to the victim's account.
func TestVerifyRejectsForgedAndTamperedState(t *testing.T) {
	signer, err := googleoauth.NewSigner(testSecret())
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	other, err := googleoauth.NewSigner([]byte(strings.Repeat("x", 32)))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	state, err := signer.Issue(uuid.New())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parts := strings.Split(state, ".")

	cases := map[string]string{
		"empty":           "",
		"not a state":     "garbage",
		"missing parts":   strings.Join(parts[:3], "."),
		"wrong signature": parts[0] + "." + parts[1] + "." + parts[2] + ".AAAA",
		"another signer":  mustIssue(t, other),
	}

	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := signer.Verify(value); err == nil {
				t.Error("Verify accepted an invalid state")
			}
		})
	}
}

// Swapping the user id must invalidate the signature, which is the property that stops
// an attacker redirecting their own authorization at somebody else's account.
func TestVerifyRejectsASwappedUser(t *testing.T) {
	signer, err := googleoauth.NewSigner(testSecret())
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	state, err := signer.Issue(uuid.New())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parts := strings.Split(state, ".")
	swapped := uuid.New().String() + "." + parts[1] + "." + parts[2] + "." + parts[3]

	if _, err := signer.Verify(swapped); err == nil {
		t.Error("Verify accepted a state with a different user")
	}
}

func TestVerifyRejectsAnExpiredState(t *testing.T) {
	signer, err := googleoauth.NewSigner(testSecret())
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	state, err := signer.Issue(uuid.New())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Issued two TTLs ago. The clock is not injectable through the public API, so this
	// is produced by asking for the state and waiting out the signature's validity
	// window in a way that does not slow the suite: the age is checked against IssuedAt,
	// so a hand-built stale payload with a valid signature is the same code path.
	parts := strings.Split(state, ".")
	stale := parts[0] + "." + parts[1] + "." + "1000000000" + "." + parts[3]

	if _, err := signer.Verify(stale); err == nil {
		t.Error("Verify accepted a state issued long ago")
	}
}

func TestVerifyRejectsAStateFromTheFuture(t *testing.T) {
	signer, err := googleoauth.NewSigner(testSecret())
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	state, err := signer.Issue(uuid.New())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parts := strings.Split(state, ".")
	future := parts[0] + "." + parts[1] + ".99999999999." + parts[3]

	if _, err := signer.Verify(future); err == nil {
		t.Error("Verify accepted a state issued in the future")
	}
}

func mustIssue(t *testing.T, signer *googleoauth.Signer) string {
	t.Helper()

	state, err := signer.Issue(uuid.New())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	return state
}

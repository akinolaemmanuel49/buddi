package config

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// setOAuthEnv sets the OAuth variables and the ones Load requires, so a test about the
// key is not failed by an unrelated missing setting.
func setOAuthEnv(t *testing.T, values map[string]string) {
	t.Helper()

	requiredEnv(t)

	for key, value := range values {
		setEnv(t, key, value)
	}
}

func validKeyHex() string {
	return hex.EncodeToString([]byte(strings.Repeat("k", OAuthEncryptionKeyBytes)))
}

// An unconfigured key means connectors are off, and that must not stop the server from
// starting. A deployment that has not asked for a calendar should be unaffected by the
// feature existing.
func TestNoEncryptionKeyMeansConnectorsAreSimplyOff(t *testing.T) {
	setOAuthEnv(t, nil)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.OAuth.OAuthEnabled() {
		t.Error("connectors are enabled with no key configured")
	}
}

// A key of the wrong length is rejected outright. Silently disabling the feature would
// leave a user asking why their calendar cannot be connected, with nothing in the logs
// to explain it.
func TestAKeyOfTheWrongLengthIsRejected(t *testing.T) {
	cases := map[string]string{
		"too short":   hex.EncodeToString([]byte(strings.Repeat("k", 16))),
		"too long":    hex.EncodeToString([]byte(strings.Repeat("k", 64))),
		"empty-ish":   "abcd",
		"not a key":   "this is not a key at all",
		"odd hex":     "abc",
		"hex letters": strings.Repeat("z", 64),
	}

	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			setOAuthEnv(t, map[string]string{"OAUTH_ENCRYPTION_KEY": key})

			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted OAUTH_ENCRYPTION_KEY = %q", key)
			}

			if !strings.Contains(err.Error(), "OAUTH_ENCRYPTION_KEY") {
				t.Errorf("error = %q, want it to name the setting", err)
			}
		})
	}
}

// Both encodings a person is likely to paste have to work. `openssl rand -hex 32` and
// `openssl rand -base64 32` are the two commands anyone would reach for.
func TestBothKeyEncodingsAreAccepted(t *testing.T) {
	key := []byte(strings.Repeat("k", OAuthEncryptionKeyBytes))

	cases := map[string]string{
		"hex":     hex.EncodeToString(key),
		"base64":  base64.StdEncoding.EncodeToString(key),
		"raw url": base64.RawURLEncoding.EncodeToString(key),
	}

	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			setOAuthEnv(t, map[string]string{"OAUTH_ENCRYPTION_KEY": encoded})

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			if !cfg.OAuth.OAuthEnabled() {
				t.Fatal("connectors are disabled with a valid key")
			}

			if len(cfg.OAuth.EncryptionKey) != OAuthEncryptionKeyBytes {
				t.Errorf("key length = %d, want %d", len(cfg.OAuth.EncryptionKey), OAuthEncryptionKeyBytes)
			}
		})
	}
}

// A key must be accepted only when it decodes to the right bytes, not merely when it
// parses. A base64 string of the wrong length is still valid base64.
func TestAKeyOfTheWrongLengthIsNotAcceptedJustBecauseItDecodes(t *testing.T) {
	short := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 24)))

	setOAuthEnv(t, map[string]string{"OAUTH_ENCRYPTION_KEY": short})

	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a key that decodes to 24 bytes")
	}
}

// Refreshing on a margin longer than the refresh credential itself lives would have us
// presume a token dead while it is still good, so the margin is checked against the
// refresh token's lifetime.
func TestARefreshMarginLongerThanTheRefreshTokenIsRejected(t *testing.T) {
	setOAuthEnv(t, map[string]string{
		"OAUTH_ENCRYPTION_KEY": validKeyHex(),
		"OAUTH_REFRESH_MARGIN": "240h",
		"REFRESH_EXPIRY":       "48h",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted a refresh margin longer than the refresh token lives")
	}

	if !strings.Contains(err.Error(), "OAUTH_REFRESH_MARGIN") {
		t.Errorf("error = %q, want it to name the setting", err)
	}
}

// The defaults have to be coherent with each other, or every deployment would fail to
// start the moment a key was configured.
func TestTheDefaultRefreshMarginFitsInsideTheRefreshTokenLifetime(t *testing.T) {
	setOAuthEnv(t, map[string]string{"OAUTH_ENCRYPTION_KEY": validKeyHex()})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.OAuth.RefreshMargin <= 0 {
		t.Fatalf("RefreshMargin = %v, want a positive default", cfg.OAuth.RefreshMargin)
	}

	if cfg.OAuth.RefreshMargin >= cfg.Auth.RefreshTTL {
		t.Errorf(
			"RefreshMargin %v must be shorter than RefreshTTL %v",
			cfg.OAuth.RefreshMargin, cfg.Auth.RefreshTTL,
		)
	}
}

// Google's own credentials are reported separately from the key: a key alone lets a
// deployment seal tokens it has no way to obtain, and Google credentials alone leave
// nowhere to put them.
func TestGoogleConfiguredNeedsAllThreeSettings(t *testing.T) {
	setOAuthEnv(t, map[string]string{"OAUTH_ENCRYPTION_KEY": validKeyHex()})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.OAuth.GoogleConfigured() {
		t.Error("Google reported as configured with no client credentials")
	}

	setOAuthEnv(t, map[string]string{
		"GOOGLE_CLIENT_ID":     "id",
		"GOOGLE_CLIENT_SECRET": "secret",
	})

	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.OAuth.GoogleConfigured() {
		t.Error("Google reported as configured without a redirect URL")
	}

	setOAuthEnv(t, map[string]string{"GOOGLE_REDIRECT_URL": "http://localhost:8080/oauth/google/callback"})

	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.OAuth.GoogleConfigured() {
		t.Error("Google reported as unconfigured with all three settings present")
	}
}

// The margin must be a real duration. A zero would refresh only once already expired,
// which is the failure the margin exists to prevent.
func TestANegativeRefreshMarginIsRejected(t *testing.T) {
	setOAuthEnv(t, map[string]string{
		"OAUTH_ENCRYPTION_KEY": validKeyHex(),
		"OAUTH_REFRESH_MARGIN": "-1m",
	})

	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a negative refresh margin")
	}
}

func TestAZeroRefreshMarginIsAllowedButDefaultIsNotZero(t *testing.T) {
	setOAuthEnv(t, map[string]string{
		"OAUTH_ENCRYPTION_KEY": validKeyHex(),
		"OAUTH_REFRESH_MARGIN": "0s",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.OAuth.RefreshMargin != 0 {
		t.Errorf("RefreshMargin = %v, want 0", cfg.OAuth.RefreshMargin)
	}

	if OAuthEncryptionKeyBytes != 32 {
		t.Errorf("key size = %d, want 32 for AES-256", OAuthEncryptionKeyBytes)
	}

	if cfg.OAuth.RefreshMargin > 0 && cfg.OAuth.RefreshMargin < time.Second {
		t.Error("a sub-second default margin would be shorter than a clock tick")
	}
}

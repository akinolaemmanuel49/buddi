// Package oauth stores and refreshes third-party credentials.
//
// The seam exists before any provider is wired to it, and that is the point: a
// refresh token is a long-lived credential for a user's real account, so the threat
// model is decided and tested while nothing real exists to leak. Everything here
// works against a Provider interface with a fake in the tests, so a real Google
// implementation is a wiring change rather than a design change.
//
// Two rules shape the package. A token is encrypted before it is stored and decrypted
// only in memory, so the database never holds a usable credential. And a token is
// scoped to one user on every read and write, so a provider name is never a
// sufficient lookup.
package oauth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// ErrNotConnected reports that a user has not connected a provider.
var ErrNotConnected = errors.New("oauth: provider is not connected")

// ErrRevoked reports that a credential exists but has been disconnected.
var ErrRevoked = errors.New("oauth: provider was disconnected")

// Cipher seals and opens token material at rest.
//
// AES-256-GCM rather than a bare encryption key: the authentication tag is what makes
// a tampered ciphertext fail loudly instead of decrypting to an attacker-chosen
// token, which would be a credential for a real account that this server then uses.
type Cipher interface {
	// Seal returns an encoded, authenticated ciphertext.
	Seal(plaintext []byte) (string, error)

	// Open reverses Seal.
	Open(encoded string) ([]byte, error)
}

// AESCipher is the production Cipher.
type AESCipher struct {
	aead cipher.AEAD
}

// keySize is AES-256. 32 bytes rather than 16 because the key protects a live
// account credential, and 128 bits is not a margin worth trading for.
const keySize = 32

// NewAESCipher builds a cipher from a 32 byte key.
func NewAESCipher(key []byte) (*AESCipher, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("oauth: encryption key must be %d bytes, got %d", keySize, len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("oauth: could not build cipher: %w", err)
	}

	// The nonce is prepended to the ciphertext rather than derived, and a fresh one is
	// generated per seal. Reusing a nonce under the same key breaks GCM outright, and
	// deriving it deterministically from the record would make that easy to do by
	// accident on the second write to the same row.
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("oauth: could not build GCM: %w", err)
	}

	return &AESCipher{aead: gcm}, nil
}

// Seal encrypts plaintext and returns standard base64.
func (c *AESCipher) Seal(plaintext []byte) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("oauth: could not generate nonce: %w", err)
	}

	sealed := c.aead.Seal(nonce, nonce, plaintext, nil)

	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a value produced by Seal.
func (c *AESCipher) Open(encoded string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("oauth: stored token is not valid base64: %w", err)
	}

	nonceSize := c.aead.NonceSize()
	if len(raw) < nonceSize {
		return nil, fmt.Errorf("oauth: stored token is shorter than its nonce")
	}

	plaintext, err := c.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		// A wrong key and a tampered row are indistinguishable here, which is the
		// point of the authentication tag. The message says so rather than guessing.
		return nil, fmt.Errorf("oauth: could not decrypt token: %w", err)
	}

	return plaintext, nil
}

// Provider exchanges a refresh token for a new access token.
//
// This is the seam a real Google implementation fills. It is deliberately narrow:
// the provider never sees the database, and the package never sees the provider's
// client credentials.
type Provider interface {
	// Name identifies the provider being refreshed.
	Name() domain.Provider

	// Refresh exchanges a refresh token for a new access token. An empty
	// returnedRefreshToken means the provider did not rotate it and the existing one
	// stands.
	Refresh(ctx context.Context, refreshToken string) (*RefreshedToken, error)
}

// RefreshedToken is what a refresh returned.
type RefreshedToken struct {
	AccessToken  string
	RefreshToken string
	Scopes       string
	ExpiresAt    time.Time
}

// Store persists credentials.
type Store interface {
	Get(ctx context.Context, userID uuid.UUID, provider domain.Provider) (*domain.OAuthToken, error)
	Save(ctx context.Context, token *domain.OAuthToken) error
}

// Options configures a Service.
type Options struct {
	// RefreshMargin is how long before expiry a token is refreshed. Refreshing at
	// expiry spends the call: by the time the token reaches the provider it is already
	// dead, and the user sees a failure for something that was working a moment ago.
	RefreshMargin time.Duration

	// Clock is injectable so the refresh decision is testable at its exact boundary.
	Clock func() time.Time
}

// defaultRefreshMargin matches the provider's own typical lead time: Google access
// tokens last an hour, and refreshing a minute early costs one extra round trip
// while refreshing late risks the call that follows it.
const defaultRefreshMargin = 2 * time.Minute

// Service stores, retrieves and refreshes credentials.
type Service struct {
	store     Store
	providers map[domain.Provider]Provider
	cipher    Cipher

	margin time.Duration
	now    func() time.Time
}

// NewService builds the service.
func NewService(store Store, cipher Cipher, providers []Provider, opts Options) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("oauth: store is required")
	}

	if cipher == nil {
		return nil, fmt.Errorf("oauth: cipher is required")
	}

	registry := make(map[domain.Provider]Provider, len(providers))

	for _, provider := range providers {
		if provider == nil {
			return nil, fmt.Errorf("oauth: nil provider")
		}

		name := provider.Name()
		if !name.IsValid() {
			return nil, fmt.Errorf("oauth: provider %q is not a known provider", name)
		}

		if _, exists := registry[name]; exists {
			return nil, fmt.Errorf("oauth: duplicate provider %q", name)
		}

		registry[name] = provider
	}

	service := &Service{
		store:     store,
		providers: registry,
		cipher:    cipher,
		margin:    opts.RefreshMargin,
		now:       opts.Clock,
	}

	if service.margin <= 0 {
		service.margin = defaultRefreshMargin
	}

	if service.now == nil {
		service.now = time.Now
	}

	return service, nil
}

// Grant is what a provider's authorisation response yielded.
//
// It arrives as plaintext and is sealed before it is stored, so no field here is
// ever persisted.
type Grant struct {
	AccessToken  string
	RefreshToken string
	Scopes       string
	ExpiresAt    time.Time
}

// Connect stores a freshly granted credential, encrypted.
func (s *Service) Connect(ctx context.Context, userID uuid.UUID, provider domain.Provider, grant Grant) error {
	if grant.AccessToken == "" {
		return domain.Invalid("an access token is required", nil)
	}

	if grant.RefreshToken == "" {
		return domain.Invalid("a refresh token is required", nil)
	}

	access, err := s.cipher.Seal([]byte(grant.AccessToken))
	if err != nil {
		return err
	}

	refresh, err := s.cipher.Seal([]byte(grant.RefreshToken))
	if err != nil {
		return err
	}

	record, err := domain.NewOAuthToken(
		userID,
		provider,
		access,
		refresh,
		grant.Scopes,
		grant.ExpiresAt,
		s.now(),
	)
	if err != nil {
		return err
	}

	return s.store.Save(ctx, record)
}

// Connection describes a stored credential without revealing it.
type Connection struct {
	Provider  domain.Provider `json:"provider"`
	Connected bool            `json:"connected"`
	Scopes    []string        `json:"scopes"`
	ExpiresAt time.Time       `json:"expires_at"`
}

// Status reports whether a provider is connected, without decrypting anything.
//
// It reads only the ciphertext-free fields, so it is safe on a page a user loads
// routinely: there is no reason to unseal a refresh token to draw a "connected"
// badge, and not doing so keeps the plaintext out of a code path that runs often.
func (s *Service) Status(ctx context.Context, userID uuid.UUID, provider domain.Provider) (Connection, error) {
	record, err := s.store.Get(ctx, userID, provider)
	if err != nil {
		if isNotFound(err) {
			return Connection{Provider: provider}, ErrNotConnected
		}

		return Connection{Provider: provider}, err
	}

	if !record.Connected(s.now()) {
		return Connection{Provider: provider, ExpiresAt: record.ExpiresAt}, ErrRevoked
	}

	return Connection{
		Provider:  record.Provider,
		Connected: true,
		Scopes:    record.ScopeList(),
		ExpiresAt: record.ExpiresAt,
	}, nil
}

// Disconnect revokes a credential. The row is kept so the disconnect is auditable
// and a later reconnect is distinguishable from one that never happened.
func (s *Service) Disconnect(ctx context.Context, userID uuid.UUID, provider domain.Provider) error {
	record, err := s.store.Get(ctx, userID, provider)
	if err != nil {
		return err
	}

	now := s.now()
	record.Revoke(now)

	return s.store.Save(ctx, record)
}

// AccessToken returns a usable access token, refreshing it first if it is close to
// expiry.
//
// The returned string is plaintext and belongs to the caller alone. It is returned
// rather than cached here so that there is no long-lived plaintext sitting in this
// process, which is the one place a credential would be reachable without the key.
func (s *Service) AccessToken(ctx context.Context, userID uuid.UUID, provider domain.Provider) (string, error) {
	record, err := s.store.Get(ctx, userID, provider)
	if err != nil {
		if isNotFound(err) {
			return "", ErrNotConnected
		}

		return "", err
	}

	now := s.now()

	if !record.Connected(now) {
		return "", ErrRevoked
	}

	if record.Refreshable(now, s.margin) {
		if err := s.refresh(ctx, record, now); err != nil {
			return "", err
		}

		now = s.now()
	}

	access, err := s.open(record.AccessTokenCiphertext)
	if err != nil {
		return "", err
	}

	if !record.ExpiresAt.After(now) {
		// The stored token expired between the refresh check and here, which means the
		// refresh did not take. Reporting that beats handing back a token that is
		// already dead, because the caller would discover it as an opaque provider
		// rejection instead of as an expired credential.
		return "", fmt.Errorf("oauth: %s access token expired and could not be refreshed", provider)
	}

	return access, nil
}

// HasProvider reports whether the service can renew credentials for provider.
//
// Exists so a composition root can assert the wiring at start-up rather than
// discovering it when a user approves something an hour into their session: the
// provider list is captured by value when the service is built, so passing an empty
// one compiles, starts, connects, and fails only at the first refresh.
func (s *Service) HasProvider(provider domain.Provider) bool {
	// Nil-safe on purpose: this is a diagnostic, and a misordered composition root
	// should produce a warning naming the problem rather than a nil dereference that
	// looks like an unrelated crash.
	if s == nil {
		return false
	}

	_, ok := s.providers[provider]

	return ok
}

// refresh exchanges the stored refresh token and writes the result back.
func (s *Service) refresh(ctx context.Context, record *domain.OAuthToken, now time.Time) error {
	exchange, ok := s.providers[record.Provider]
	if !ok {
		return fmt.Errorf("oauth: no refresh implementation for %s", record.Provider)
	}

	refreshToken, err := s.open(record.RefreshTokenCiphertext)
	if err != nil {
		return err
	}

	refreshed, err := exchange.Refresh(ctx, refreshToken)
	if err != nil {
		return err
	}

	if refreshed == nil || refreshed.AccessToken == "" {
		return fmt.Errorf("oauth: %s returned no access token", record.Provider)
	}

	access, err := s.cipher.Seal([]byte(refreshed.AccessToken))
	if err != nil {
		return err
	}

	// A provider that does not rotate the refresh token returns nothing, and writing
	// that empty value would make the credential permanently unrefreshable the moment
	// the access token expired. Only replace it when one was actually returned.
	sealedRefresh := ""
	if refreshed.RefreshToken != "" {
		sealedRefresh, err = s.cipher.Seal([]byte(refreshed.RefreshToken))
		if err != nil {
			return err
		}
	}

	if err := record.ApplyRefresh(access, sealedRefresh, refreshed.Scopes, refreshed.ExpiresAt, now); err != nil {
		return err
	}

	return s.store.Save(ctx, record)
}

// open decrypts one stored value.
func (s *Service) open(encoded string) (string, error) {
	plaintext, err := s.cipher.Open(encoded)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}

// isNotFound reports whether err means the row does not exist.
//
// The persistence layer aliases its own not-found to the domain's, so one check
// covers both a fake store and the real repository without this package importing
// the persistence package.
func isNotFound(err error) bool {
	return errors.Is(err, ErrNotConnected) || errors.Is(err, domain.ErrNotFound)
}

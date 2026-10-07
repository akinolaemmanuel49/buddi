package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims is the verified content of an access token.
type Claims struct {
	UserID    uuid.UUID
	IssuedAt  time.Time
	ExpiresAt time.Time
	TokenID   string
}

// TokenIssuer mints and verifies access tokens.
type TokenIssuer interface {
	Issue(userID uuid.UUID, ttl time.Duration) (string, Claims, error)
	Verify(token string) (Claims, error)
}

// JWTIssuer issues HS256 signed tokens.
//
// HS256 rather than an asymmetric algorithm because the same process both signs
// and verifies, and a single secret is one less thing to provision. The signing
// method is checked explicitly on verification, because the JWT library would
// otherwise be willing to accept an alg field that names a different algorithm.
type JWTIssuer struct {
	secret []byte
	issuer string
	now    func() time.Time
}

var _ TokenIssuer = (*JWTIssuer)(nil)

// NewJWTIssuer returns an issuer signing with secret.
func NewJWTIssuer(secret []byte, issuer string) *JWTIssuer {
	if len(secret) == 0 {
		panic("auth: signing secret is required")
	}

	if issuer == "" {
		issuer = "buddi"
	}

	return &JWTIssuer{secret: secret, issuer: issuer, now: time.Now}
}

// SetClock replaces the clock. It exists for tests and must not be called
// concurrently with issuing.
func (i *JWTIssuer) SetClock(now func() time.Time) {
	if now != nil {
		i.now = now
	}
}

// jwtClaims is the wire form. Only the subject is needed to resolve the user,
// and it is written as a string because UUID is not a JSON primitive.
type jwtClaims struct {
	jwt.RegisteredClaims
}

// Issue mints a token for userID.
func (i *JWTIssuer) Issue(userID uuid.UUID, ttl time.Duration) (string, Claims, error) {
	now := i.now().UTC()
	expiresAt := now.Add(ttl)

	registered := jwt.RegisteredClaims{
		Issuer:    i.issuer,
		Subject:   userID.String(),
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(expiresAt),
		ID:        uuid.NewString(),
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwtClaims{RegisteredClaims: registered}).
		SignedString(i.secret)
	if err != nil {
		return "", Claims{}, fmt.Errorf("auth: sign token: %w", err)
	}

	return signed, Claims{
		UserID:    userID,
		IssuedAt:  now,
		ExpiresAt: expiresAt,
		TokenID:   registered.ID,
	}, nil
}

// Verify checks a token's signature and lifetime and returns its claims.
func (i *JWTIssuer) Verify(token string) (Claims, error) {
	parsed, err := jwt.ParseWithClaims(token, &jwtClaims{}, func(t *jwt.Token) (any, error) {
		// Refuse anything that is not HS256. Without this check an attacker could
		// present an unsigned or differently signed token and have the alg field
		// believed.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("auth: unexpected signing method %v", t.Header["alg"])
		}

		return i.secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(i.issuer),
		jwt.WithTimeFunc(i.now),
	)
	if err != nil {
		return Claims{}, fmt.Errorf("auth: verify token: %w", err)
	}

	claims, ok := parsed.Claims.(*jwtClaims)
	if !ok || !parsed.Valid {
		return Claims{}, errors.New("auth: token is not valid")
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Claims{}, fmt.Errorf("auth: token subject: %w", err)
	}

	result := Claims{UserID: userID, TokenID: claims.ID}
	if claims.IssuedAt != nil {
		result.IssuedAt = claims.IssuedAt.Time
	}

	if claims.ExpiresAt != nil {
		result.ExpiresAt = claims.ExpiresAt.Time
	}

	return result, nil
}

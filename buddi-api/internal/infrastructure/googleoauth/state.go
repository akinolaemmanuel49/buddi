package googleoauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// StateTTL is how long an authorization state stays valid.
//
// Long enough for a user who has to find a verification email, short enough that a
// state captured from a log or a referrer header is useless by the time it could be
// replayed.
const StateTTL = 15 * time.Minute

// ErrInvalidState reports a state that is malformed, forged or expired.
//
// They are deliberately indistinguishable to the caller. Telling an attacker which
// of the three it was would make guessing the rest easier, and the legitimate user
// cannot act on the difference: they are asked to start again either way.
var ErrInvalidState = errors.New("googleoauth: the authorization state is not valid")

// State is the verified contents of a state parameter.
type State struct {
	UserID uuid.UUID
	// Nonce makes two states for the same user distinct, so a state cannot be
	// replayed by starting a second authorization while the first is still open.
	Nonce    string
	IssuedAt time.Time
}

// Signer issues and verifies state values.
//
// The state is signed rather than stored because an authorization round trip leaves
// the process: the browser goes to Google and comes back, and there is no session on
// the far side to look a nonce up in. Signing makes the value self-contained and
// unforgeable, which is what stops an attacker from starting an authorization and
// having the callback attach their calendar to the victim's account.
type Signer struct {
	secret []byte
	now    func() time.Time
}

// NewSigner builds a Signer.
//
// The secret must be at least 32 bytes. A short one would let a state be forged by
// brute force, which is the entire thing the signature prevents.
func NewSigner(secret []byte) (*Signer, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("googleoauth: the state signing secret must be at least 32 bytes, got %d", len(secret))
	}

	return &Signer{secret: secret, now: time.Now}, nil
}

// Issue returns a signed state for userID.
func (s *Signer) Issue(userID uuid.UUID) (string, error) {
	if userID == uuid.Nil {
		return "", errors.New("googleoauth: a user is required to issue state")
	}

	nonce, err := randomNonce()
	if err != nil {
		return "", err
	}

	issued := s.now().UTC().Unix()
	payload := fmt.Sprintf("%s.%s.%d", userID.String(), nonce, issued)

	return payload + "." + s.sign(payload), nil
}

// Verify checks a state value and returns what it carries.
//
// Every failure is ErrInvalidState on purpose; see that declaration.
func (s *Signer) Verify(value string) (State, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 4 {
		return State{}, ErrInvalidState
	}

	payload := strings.Join(parts[:3], ".")
	provided := parts[3]

	// Constant time, so a caller cannot learn the expected signature one byte at a
	// time. hmac.Equal is that comparison.
	if !hmac.Equal([]byte(provided), []byte(s.sign(payload))) {
		return State{}, ErrInvalidState
	}

	userID, err := uuid.Parse(parts[0])
	if err != nil || userID == uuid.Nil {
		return State{}, ErrInvalidState
	}

	issued, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return State{}, ErrInvalidState
	}

	issuedAt := time.Unix(issued, 0).UTC()

	// Checked in both directions: a state from the future is as suspect as an old
	// one, and clock skew on a freshly started request should not fail it.
	age := s.now().UTC().Sub(issuedAt)
	if age < 0 || age > StateTTL {
		return State{}, ErrInvalidState
	}

	return State{UserID: userID, Nonce: parts[1], IssuedAt: issuedAt}, nil
}

func (s *Signer) sign(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))

	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func randomNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("googleoauth: could not generate a nonce: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

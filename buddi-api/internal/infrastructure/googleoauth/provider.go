// Package googleoauth implements the authorization-code flow for Google.
//
// It is the only place that knows Google exists. The oauth service above it stores
// and refreshes credentials and never talks to a provider, so without this package
// there is a way to keep a token but no way to obtain the first one.
package googleoauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/oauth"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// Google's OAuth endpoints. Overridable so tests can point at a stub.
const (
	DefaultAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	DefaultTokenURL = "https://oauth2.googleapis.com/token"
)

// Scopes are what Buddi asks Google for.
//
// Calendar read and write only. It is tempting to also request offline access
// explicitly, but Google issues a refresh token for installed apps on first consent
// regardless, and asking for more scope than the feature needs is how an app ends up
// with a consent screen that says it can "see all your calendars" for no reason.
const ScopeCalendar = "https://www.googleapis.com/auth/calendar"

// ProviderName is the provider this package connects.
const ProviderName = domain.ProviderGoogleCalendar

// Config holds the OAuth application's credentials.
type Config struct {
	// ClientID identifies the OAuth application.
	ClientID string

	// ClientSecret is required by Google for the code exchange. Google's installed
	// app flow can omit it, but the web flow cannot.
	ClientSecret string

	// RedirectURL is where Google returns the user. It must match exactly what is
	// registered with Google, or consent fails with an opaque redirect_uri mismatch.
	RedirectURL string

	// AuthURL and TokenURL override Google's endpoints. Tests only.
	AuthURL  string
	TokenURL string

	// HTTPClient replaces the underlying client.
	HTTPClient *http.Client
}

// Configured reports whether there is enough configuration to run a flow.
//
// Reported rather than assumed, because a half-configured connector is worse than an
// absent one: it registers a tool whose every call fails on a credential it cannot
// obtain.
func (c Config) Configured() bool {
	return strings.TrimSpace(c.ClientID) != "" &&
		strings.TrimSpace(c.ClientSecret) != "" &&
		strings.TrimSpace(c.RedirectURL) != ""
}

// Provider authorizes and refreshes Google credentials.
type Provider struct {
	config Config
	client *http.Client
}

var _ oauth.Provider = (*Provider)(nil)

// New builds a Provider.
func New(config Config) (*Provider, error) {
	if !config.Configured() {
		return nil, fmt.Errorf("googleoauth: client id, client secret and redirect url are all required")
	}

	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}

	provider := &Provider{config: config, client: client}

	if _, err := url.Parse(provider.authURL()); err != nil {
		return nil, fmt.Errorf("googleoauth: %q is not a usable authorization url", provider.authURL())
	}

	if _, err := url.Parse(provider.tokenURL()); err != nil {
		return nil, fmt.Errorf("googleoauth: %q is not a usable token url", provider.tokenURL())
	}

	return provider, nil
}

func (p *Provider) authURL() string {
	if strings.TrimSpace(p.config.AuthURL) == "" {
		return DefaultAuthURL
	}

	return p.config.AuthURL
}

func (p *Provider) tokenURL() string {
	if strings.TrimSpace(p.config.TokenURL) == "" {
		return DefaultTokenURL
	}

	return p.config.TokenURL
}

// Name identifies the provider to the oauth service.
func (p *Provider) Name() domain.Provider { return ProviderName }

// AuthCodeURL builds the URL to send the user to for consent.
//
// accessType=offline is what makes Google return a refresh token at all; without it
// the response carries a one-hour access token and nothing to renew it with, which
// looks like a working connection that dies silently an hour later.
//
// prompt=consent forces the consent screen even for a user who has already approved,
// because Google otherwise omits the refresh token from a second grant and the
// credential cannot be renewed.
func (p *Provider) AuthCodeURL(state string) string {
	query := url.Values{}
	query.Set("client_id", p.config.ClientID)
	query.Set("redirect_uri", p.config.RedirectURL)
	query.Set("response_type", "code")
	query.Set("scope", ScopeCalendar)
	query.Set("access_type", "offline")
	query.Set("prompt", "consent")
	query.Set("include_granted_scopes", "true")
	query.Set("state", state)

	return p.authURL() + "?" + query.Encode()
}

// Exchange trades an authorization code for tokens.
func (p *Provider) Exchange(ctx context.Context, code string) (*oauth.Grant, error) {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return nil, fmt.Errorf("googleoauth: an authorization code is required")
	}

	form := url.Values{}
	form.Set("code", trimmed)
	form.Set("client_id", p.config.ClientID)
	form.Set("client_secret", p.config.ClientSecret)
	form.Set("redirect_uri", p.config.RedirectURL)
	form.Set("grant_type", "authorization_code")

	token, err := p.post(ctx, form)
	if err != nil {
		return nil, err
	}

	// Google only sends a refresh token on the first grant, and only when consent was
	// forced. Without one the credential cannot be renewed, so it is refused here
	// rather than stored as something that expires in an hour.
	if token.RefreshToken == "" {
		return nil, errors.New(
			"google: no refresh token was returned; revoke the app's access and connect again",
		)
	}

	return &oauth.Grant{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		Scopes:       token.Scope,
		ExpiresAt:    token.expiry(),
	}, nil
}

// Refresh exchanges a refresh token for a new access token, satisfying
// oauth.Provider.
func (p *Provider) Refresh(ctx context.Context, refreshToken string) (*oauth.RefreshedToken, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, errors.New("googleoauth: a refresh token is required")
	}

	form := url.Values{}
	form.Set("client_id", p.config.ClientID)
	form.Set("client_secret", p.config.ClientSecret)
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")

	token, err := p.post(ctx, form)
	if err != nil {
		return nil, err
	}

	return &oauth.RefreshedToken{
		AccessToken: token.AccessToken,
		// Google does not rotate refresh tokens. An empty value here means "keep the
		// one you have", which is exactly what the oauth service expects.
		RefreshToken: token.RefreshToken,
		Scopes:       token.Scope,
		ExpiresAt:    token.expiry(),
	}, nil
}

// tokenResponse is Google's token endpoint body.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`

	// Google reports failures with HTTP 400 and this body.
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// expiry converts expires_in into an instant.
//
// A response with no expires_in is treated as already expired rather than as
// immortal: an access token with no known lifetime should be refreshed before use,
// not trusted until it fails.
func (t tokenResponse) expiry() time.Time {
	if t.ExpiresIn <= 0 {
		return time.Now().Add(-time.Minute)
	}

	return time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
}

// maxErrorBytes caps an error body. Google's are small; this stops a misbehaving
// endpoint from being read into memory without limit.
const maxErrorBytes = 64 << 10

func (p *Provider) post(ctx context.Context, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, p.tokenURL(), strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("googleoauth: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	res, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("googleoauth: token request: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, tokenError(res)
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, maxErrorBytes))
	if err != nil {
		return nil, fmt.Errorf("googleoauth: read token response: %w", err)
	}

	var token tokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("googleoauth: decode token response: %w", err)
	}

	if token.AccessToken == "" {
		return nil, errors.New("googleoauth: the token response carried no access token")
	}

	return &token, nil
}

// tokenError reports Google's own explanation.
//
// Preferred over a status code because Google's failures are specific and a bare 400
// is not actionable: invalid_grant means the refresh token is dead, access_denied
// means the user said no.
func tokenError(res *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBytes))

	var parsed struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}

	if json.Unmarshal(body, &parsed) == nil && parsed.Error != "" {
		if parsed.ErrorDescription != "" {
			return fmt.Errorf("googleoauth: %s: %s", parsed.Error, parsed.ErrorDescription)
		}

		return fmt.Errorf("googleoauth: %s", parsed.Error)
	}

	return fmt.Errorf("googleoauth: %s", res.Status)
}

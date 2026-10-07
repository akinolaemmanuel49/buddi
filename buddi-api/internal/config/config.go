// Package config loads and validates the runtime configuration from the
// environment. Every secret is required rather than defaulted, so a
// misconfigured deployment fails at startup instead of at first request.
package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	Port           string
	Environment    string
	LogLevel       string
	AllowedOrigins []string
	WebOrigin      string
	RequestTimeout time.Duration
	ShutdownGrace  time.Duration

	Auth      Auth
	Database  Database
	Ollama    Ollama
	Agent     Agent
	MCP       MCP
	OAuth     OAuth
	Telemetry Telemetry
	Retrieval Retrieval
}

// Auth holds token issuance settings. Access tokens are signed; refresh tokens
// are opaque random strings stored only as hashes, so there is no refresh
// signing secret and nothing that could mint a refresh token from the database.
type Auth struct {
	Issuer       string
	AccessSecret []byte
	AccessTTL    time.Duration
	RefreshTTL   time.Duration
	BcryptCost   int
}

// Database holds connection pool and migration settings.
type Database struct {
	DSN             string
	MigrateOnStart  bool
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	SlowQueryAfter  time.Duration
}

// Ollama holds the local model gateway settings. Model, PlannerModel and
// EmbedModel are switched purely by environment variable, so a different model
// needs no code change. There is deliberately only one general-purpose model in
// use; see defaultModel.
type Ollama struct {
	BaseURL       string
	Model         string
	PlannerModel  string
	EmbedModel    string
	ContextLength int
	Temperature   float64
	KeepAlive     time.Duration
	Timeout       time.Duration
	// PlanRetries is how many times a schema-invalid plan is re-asked with the
	// validation errors appended. Small models need this; large ones rarely use
	// it, but it is the difference between a retry and a hard failure.
	PlanRetries int
	// PlannerThink caps the reasoning effort the planner asks for. It is a level
	// rather than a bool because the runtime refuses think=false alongside a
	// schema, which is the only way to get structured output cheaply.
	//
	// Empty means "send no think field at all", which is what a non-thinking
	// instruct model needs: it exposes no reasoning to cap, and asking for a level
	// it does not implement is at best ignored. This is why the setting exists
	// rather than being hardcoded: the planner model is switchable by environment
	// variable, so a deployment that swaps in an instruct model has to be able to
	// drop the reasoning request too, without editing code.
	PlannerThink string
}

// Agent holds the settings for the orchestrator.
//
// Both planning and execution are answered synchronously, so PlanTimeout is not
// an internal detail: if it exceeds the server's write timeout the client gets a
// truncated response and the run's outcome is never recorded.
type Agent struct {
	PlanTimeout time.Duration
}

// MCP holds the settings for the stdio tool server the API spawns per run.
type MCP struct {
	BinaryPath   string
	ServiceToken string
	ToolTimeout  time.Duration
	RunTimeout   time.Duration
}

// OAuthEncryptionKeyBytes is the required length of the token encryption key.
//
// AES-256 is used rather than AES-128 because a leaked key is not recoverable: the
// tokens it protects are live credentials for the user's account, and there is no
// second copy to fall back to. The length is a constant rather than a preference so
// the cipher is never built with a key it would have to pad or truncate.
const OAuthEncryptionKeyBytes = 32

// OAuth holds the settings for third-party account connections.
//
// It is optional. An empty EncryptionKey means no connector is registered, which is
// what keeps a deployment that has not configured Google from failing to start. The
// key is separate from SECRET_KEY and from JWT_SECRET on purpose: it protects stored
// credentials rather than session tokens, so it can be rotated without invalidating
// everybody's login, and losing it does not mean losing everybody's session.
type OAuth struct {
	// EncryptionKey seals access and refresh tokens at rest. Hex or base64, decoding
	// to exactly OAuthEncryptionKeyBytes.
	EncryptionKey []byte

	// GoogleClientID and GoogleClientSecret identify this deployment to Google.
	GoogleClientID     string
	GoogleClientSecret string

	// GoogleRedirectURL is where Google sends the user back after consent.
	GoogleRedirectURL string

	// RefreshMargin is how long before expiry a token is refreshed. Refreshing late
	// would mean a call failing at the provider because the token expired between the
	// approval and the write.
	RefreshMargin time.Duration
}

// OAuthEnabled reports whether connectors can be built.
//
// It requires the key and not just any setting, because a half-configured connector is
// worse than none: it would register a tool whose every call fails on a credential it
// cannot seal or read.
func (c OAuth) OAuthEnabled() bool {
	return len(c.EncryptionKey) == OAuthEncryptionKeyBytes
}

// GoogleConfigured reports whether Google's own credentials are present.
func (c OAuth) GoogleConfigured() bool {
	return c.GoogleClientID != "" && c.GoogleClientSecret != "" && c.GoogleRedirectURL != ""
}

// Telemetry holds OpenTelemetry settings. An empty Endpoint disables export
// and installs a no-op pipeline.
type Telemetry struct {
	Enabled     bool
	Endpoint    string
	ServiceName string
	SampleRatio float64
	Insecure    bool
}

// NoteChunkEmbeddingDimensions is the width of note_chunks.embedding.
//
// It is a constant rather than a setting because the column was declared as
// vector (768) in the initial migration and a check constraint enforces it on
// every insert. Changing the width is a schema migration, not a config change,
// so the value is validated rather than trusted.
const NoteChunkEmbeddingDimensions = 768

// Retrieval holds chunking and vector search settings. MaxInputTokens mirrors
// the embedder's own input ceiling so the chunker can never produce a segment
// the embedder would silently truncate.
type Retrieval struct {
	EmbeddingDimensions int
	ChunkTokens         int
	ChunkOverlap        int
	MaxInputTokens      int
	TopK                int
	MinSimilarity       float64

	// IndexBatchSize is how many notes one pass of the background indexer claims.
	IndexBatchSize int

	// IndexInterval is how long it waits when there is nothing to do, which sets the
	// worst-case delay between saving a note and being able to search it.
	IndexInterval time.Duration

	// IndexLease is how long a claimed note stays out of reach of other workers. It
	// has to exceed the slowest embedding call the runtime can make, or a worker
	// loses its note to another one partway through.
	IndexLease time.Duration
}

// IsDevelopment reports whether relaxed rules apply, such as allowing short
// development-only secrets.
func (c *Config) IsDevelopment() bool {
	return strings.EqualFold(c.Environment, "development")
}

// PlannerModelOrDefault returns the model used for planning, falling back to
// the general purpose model when no override is set.
func (o Ollama) PlannerModelOrDefault() string {
	if o.PlannerModel != "" {
		return o.PlannerModel
	}

	return o.Model
}

// Default values, applied when the matching variable is unset.
const (
	defaultPort        = "8000"
	defaultEnvironment = "development"
	// defaultWebOrigin is the Vite dev server, which is where the UI runs unless a
	// deployment says otherwise.
	defaultWebOrigin     = "http://localhost:5173"
	defaultLogLevel      = "info"
	defaultBcryptCost    = 12
	defaultContextLength = 8192
	defaultTemperature   = 0.2

	// defaultModel is the only general-purpose model the application uses.
	// Larger tiers were evaluated and dropped: on a machine with no GPU they
	// cost memory that buys quality this MVP does not need. Embeddings use
	// defaultEmbedModel.
	defaultModel          = "qwen3:0.6b"
	defaultEmbedModel     = "nomic-embed-text"
	defaultRequestTimeout = 60 * time.Second
	defaultShutdownGrace  = 10 * time.Second
	defaultToolTimeout    = 30 * time.Second
	defaultRunTimeout     = 5 * time.Minute
	// defaultPlanTimeout leaves room under the 60s request timeout for a slow
	// model plus a retry, while still guaranteeing the run is recorded before the
	// server abandons the response.
	defaultPlanTimeout = 45 * time.Second

	// defaultPlannerThink caps planning reasoning at the cheapest graded level.
	//
	// The default model is a reasoning model, and its default level roughly doubles
	// planning latency on a CPU. A deployment that switches BUDDI_PLANNER_MODEL to
	// a non-thinking instruct model should clear this, which is what an empty value
	// means: no think field is sent at all.
	defaultPlannerThink = "low"

	// minSigningSecretBytes is the shortest acceptable HMAC signing key.
	minSigningSecretBytes = 32

	// defaultOAuthRefreshMargin is how long before expiry a token is refreshed. Two
	// minutes is enough to cover a slow approval: the user reads the payload, and the
	// write happens when they approve it.
	defaultOAuthRefreshMargin = 2 * time.Minute
)

// Load reads the environment and returns a validated configuration.
func Load() (*Config, error) {
	cfg := &Config{
		Port:           getEnv("PORT", defaultPort),
		Environment:    getEnv("ENVIRONMENT", defaultEnvironment),
		LogLevel:       getEnv("LOG_LEVEL", defaultLogLevel),
		AllowedOrigins: splitCSV(getEnv("ALLOWED_ORIGINS", "")),
		WebOrigin:      getEnv("BUDDI_WEB_ORIGIN", defaultWebOrigin),
		Auth: Auth{
			Issuer:       getEnv("JWT_ISSUER", "buddi"),
			AccessSecret: []byte(getEnv("JWT_SECRET", "")),
			BcryptCost:   getEnvInt("BCRYPT_COST", defaultBcryptCost),
		},
		Database: Database{
			DSN:             getEnv("DATABASE_URI", ""),
			MigrateOnStart:  getEnvBool("MIGRATE_ON_START", true),
			MaxOpenConns:    getEnvInt("DB_MAX_OPEN_CONNS", 20),
			MaxIdleConns:    getEnvInt("DB_MAX_IDLE_CONNS", 5),
			SlowQueryAfter:  getEnvDuration("DB_SLOW_QUERY_THRESHOLD", 200*time.Millisecond),
			ConnMaxLifetime: getEnvDuration("DB_CONN_MAX_LIFETIME", time.Hour),
			ConnMaxIdleTime: getEnvDuration("DB_CONN_MAX_IDLE_TIME", 30*time.Minute),
		},
		Ollama: Ollama{
			BaseURL: getEnv("BUDDI_OLLAMA_BASE_URL", "http://127.0.0.1:11434"),
			// qwen3:0.6b is the only general model in use. A machine without a
			// GPU has to fit the weights and the KV cache in shared RAM, and a
			// larger tier costs memory for quality this MVP does not need.
			Model:         getEnv("BUDDI_MODEL", defaultModel),
			PlannerModel:  getEnv("BUDDI_PLANNER_MODEL", ""),
			EmbedModel:    getEnv("BUDDI_EMBED_MODEL", defaultEmbedModel),
			ContextLength: getEnvInt("BUDDI_OLLAMA_CONTEXT_LENGTH", defaultContextLength),
			Temperature:   getEnvFloat("BUDDI_OLLAMA_TEMPERATURE", defaultTemperature),
			PlanRetries:   getEnvInt("BUDDI_PLAN_MAX_RETRIES", 1),
			PlannerThink:  getEnv("BUDDI_PLANNER_THINK", defaultPlannerThink),
		},
		Agent: Agent{
			PlanTimeout: getEnvDuration("BUDDI_AGENT_PLAN_TIMEOUT", defaultPlanTimeout),
		},
		MCP: MCP{
			BinaryPath: getEnv("MCP_BINARY_PATH", "buddi-mcp-tasks"),
			// The token the spawned tool server presents on internal callbacks.
			// SECRET_KEY doubles as the fallback so a single secret covers a
			// development machine.
			ServiceToken: getEnv("BUDDI_TOOL_TOKEN", getEnv("SECRET_KEY", "")),
		},
		OAuth: OAuth{
			EncryptionKey:      decodeOAuthKey(getEnv("OAUTH_ENCRYPTION_KEY", "")),
			GoogleClientID:     getEnv("GOOGLE_CLIENT_ID", ""),
			GoogleClientSecret: getEnv("GOOGLE_CLIENT_SECRET", ""),
			GoogleRedirectURL:  getEnv("GOOGLE_REDIRECT_URL", ""),
		},
		Telemetry: Telemetry{
			Endpoint:    getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
			ServiceName: getEnv("OTEL_SERVICE_NAME", "buddi-api"),
			Insecure:    getEnvBool("OTEL_EXPORTER_OTLP_INSECURE", true),
			SampleRatio: getEnvFloat("OTEL_TRACES_SAMPLER_ARG", 1),
		},
		Retrieval: Retrieval{
			EmbeddingDimensions: getEnvInt("EMBED_DIMENSIONS", 768),
			ChunkTokens:         getEnvInt("CHUNK_TOKENS", 500),
			ChunkOverlap:        getEnvInt("CHUNK_OVERLAP", 50),
			MaxInputTokens:      getEnvInt("EMBED_MAX_TOKENS", 8192),
			TopK:                getEnvInt("RETRIEVAL_TOP_K", 5),
			MinSimilarity:       getEnvFloat("RETRIEVAL_MIN_SIMILARITY", 0.3),
			IndexBatchSize:      getEnvInt("INDEX_BATCH_SIZE", 20),
		},
	}

	var err error

	if cfg.RequestTimeout, err = getEnvDurationErr("REQUEST_TIMEOUT", defaultRequestTimeout); err != nil {
		return nil, err
	}

	if cfg.Retrieval.IndexInterval, err = getEnvDurationErr("INDEX_INTERVAL", 5*time.Second); err != nil {
		return nil, err
	}

	if cfg.Retrieval.IndexLease, err = getEnvDurationErr("INDEX_LEASE", 2*time.Minute); err != nil {
		return nil, err
	}

	if cfg.ShutdownGrace, err = getEnvDurationErr("SHUTDOWN_GRACE", defaultShutdownGrace); err != nil {
		return nil, err
	}

	if cfg.Auth.AccessTTL, err = getEnvDurationErr("JWT_EXPIRY", 15*time.Minute); err != nil {
		return nil, err
	}

	if cfg.Auth.RefreshTTL, err = getEnvDurationErr("REFRESH_EXPIRY", 168*time.Hour); err != nil {
		return nil, err
	}

	if cfg.Ollama.KeepAlive, err = getEnvDurationErr("BUDDI_OLLAMA_KEEP_ALIVE", 2*time.Minute); err != nil {
		return nil, err
	}

	if cfg.Ollama.Timeout, err = getEnvDurationErr("BUDDI_OLLAMA_TIMEOUT", 2*time.Minute); err != nil {
		return nil, err
	}

	if cfg.MCP.ToolTimeout, err = getEnvDurationErr("BUDDI_MCP_TOOL_TIMEOUT", defaultToolTimeout); err != nil {
		return nil, err
	}

	if cfg.MCP.RunTimeout, err = getEnvDurationErr("BUDDI_MCP_RUN_TIMEOUT", defaultRunTimeout); err != nil {
		return nil, err
	}

	if cfg.OAuth.RefreshMargin, err = getEnvDurationErr("OAUTH_REFRESH_MARGIN", defaultOAuthRefreshMargin); err != nil {
		return nil, err
	}

	cfg.Telemetry.Enabled = cfg.Telemetry.Endpoint != ""

	// A key that was present but unusable is an error rather than a disabled feature.
	// Silently treating a mistyped key as "no connector configured" would leave a user
	// asking why their calendar cannot be connected.
	if raw := getEnv("OAUTH_ENCRYPTION_KEY", ""); raw != "" && len(cfg.OAuth.EncryptionKey) == 0 {
		return nil, InvalidEnv("OAUTH_ENCRYPTION_KEY", fmt.Sprintf(
			"must be hex or base64 decoding to exactly %d bytes",
			OAuthEncryptionKeyBytes,
		))
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate rejects configurations that would fail later in a confusing way.
// validateWebOrigin checks that a web origin is an absolute http or https URL.
//
// An origin is scheme, host and port with no path, which is what makes it safe to
// concatenate a redirect onto: anything with a path or a query would produce a URL
// that is at best wrong and at worst sends the user somewhere unexpected after they
// have just granted a permission.
func validateWebOrigin(value string) error {
	trimmed := strings.TrimSpace(value)

	if trimmed == "" {
		return errors.New("must not be empty")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("%q is not a usable URL", value)
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("must be an http or https URL, got %q", value)
	}

	if parsed.Host == "" {
		return fmt.Errorf("%q has no host", value)
	}

	if parsed.Path != "" && parsed.Path != "/" {
		return fmt.Errorf("must be an origin with no path, got %q", value)
	}

	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("must be an origin with no query or fragment, got %q", value)
	}

	return nil
}

func (c *Config) Validate() error {
	for _, required := range []struct {
		key   string
		value string
	}{
		{"SECRET_KEY", getEnv("SECRET_KEY", "")},
		{"DATABASE_URI", c.Database.DSN},
		{"JWT_SECRET", string(c.Auth.AccessSecret)},
	} {
		if required.value == "" {
			return ErrMissingEnv(required.key)
		}
	}

	// Outside development a short signing key is a real vulnerability, not an
	// inconvenience, so refuse to start rather than quietly accept it.
	if !c.IsDevelopment() {
		if len(c.Auth.AccessSecret) < minSigningSecretBytes {
			return InvalidEnv("JWT_SECRET", fmt.Sprintf("must be at least %d bytes outside development", minSigningSecretBytes))
		}
	}

	// The OAuth callback redirects here, so an unusable value does not fail at startup
	// but at the exact moment somebody tries to connect their calendar, which is the
	// worst time to learn about it.
	if err := validateWebOrigin(c.WebOrigin); err != nil {
		return InvalidEnv("BUDDI_WEB_ORIGIN", err.Error())
	}

	if c.Auth.RefreshTTL <= c.Auth.AccessTTL {
		return InvalidEnv("REFRESH_EXPIRY", "must be longer than JWT_EXPIRY")
	}

	if c.Auth.BcryptCost < 4 || c.Auth.BcryptCost > 31 {
		return InvalidEnv("BCRYPT_COST", "must be between 4 and 31")
	}

	if c.Database.MaxOpenConns < 1 {
		return InvalidEnv("DB_MAX_OPEN_CONNS", "must be at least 1")
	}

	if c.Database.MaxIdleConns < 0 || c.Database.MaxIdleConns > c.Database.MaxOpenConns {
		return InvalidEnv("DB_MAX_IDLE_CONNS", "must be between 0 and DB_MAX_OPEN_CONNS")
	}

	if c.Ollama.Model == "" {
		return InvalidEnv("BUDDI_MODEL", "must be set")
	}

	if c.Ollama.ContextLength < 512 {
		return InvalidEnv("BUDDI_OLLAMA_CONTEXT_LENGTH", "must be at least 512")
	}

	if c.Ollama.Temperature < 0 || c.Ollama.Temperature > 2 {
		return InvalidEnv("BUDDI_OLLAMA_TEMPERATURE", "must be between 0 and 2")
	}

	if c.Ollama.PlanRetries < 0 {
		return InvalidEnv("BUDDI_PLAN_MAX_RETRIES", "must not be negative")
	}

	// An unrecognised level would otherwise be rejected at the first planning call
	// instead of at startup, which on a slow runtime means a long wait to find out
	// that a variable is misspelled.
	if think := strings.TrimSpace(c.Ollama.PlannerThink); think != "" &&
		think != "low" && think != "medium" && think != "high" {
		return InvalidEnv("BUDDI_PLANNER_THINK", "must be low, medium, high, or empty")
	}

	if c.Agent.PlanTimeout <= 0 {
		return InvalidEnv("BUDDI_AGENT_PLAN_TIMEOUT", "must be positive")
	}

	// Planning is answered synchronously, so a plan timeout at or beyond the
	// request timeout would let the server abandon the response before the run is
	// recorded. Catching it here beats a run that silently never completes.
	if c.Agent.PlanTimeout >= c.RequestTimeout {
		return InvalidEnv("BUDDI_AGENT_PLAN_TIMEOUT", "must be shorter than REQUEST_TIMEOUT")
	}

	if c.OAuth.RefreshMargin < 0 {
		return InvalidEnv("OAUTH_REFRESH_MARGIN", "must not be negative")
	}

	// The margin is compared against the refresh token's own lifetime rather than
	// against the tool timeout. The margin answers "how old is this access token before
	// we replace it", and the waiting that matters is the user's: a margin longer than
	// the refresh token lives would have us presume a credential dead while it is
	// still good.
	if c.OAuth.OAuthEnabled() && c.OAuth.RefreshMargin >= c.Auth.RefreshTTL {
		return InvalidEnv("OAUTH_REFRESH_MARGIN", "must be shorter than REFRESH_EXPIRY")
	}

	// Checked against the schema rather than merely for being positive. An
	// embedder configured to return a different width still starts, indexes
	// every note it can, and fails at the insert, so the mismatch would appear
	// as notes silently stuck in the failed state instead of as a bad setting.
	if c.Retrieval.EmbeddingDimensions != NoteChunkEmbeddingDimensions {
		return InvalidEnv("EMBED_DIMENSIONS", fmt.Sprintf(
			"must be %d: the note_chunks.embedding column is fixed width by the schema",
			NoteChunkEmbeddingDimensions,
		))
	}

	if c.Retrieval.ChunkTokens < 1 {
		return InvalidEnv("CHUNK_TOKENS", "must be at least 1")
	}

	if c.Retrieval.ChunkOverlap < 0 || c.Retrieval.ChunkOverlap >= c.Retrieval.ChunkTokens {
		return InvalidEnv("CHUNK_OVERLAP", "must be non-negative and smaller than CHUNK_TOKENS")
	}

	if c.Retrieval.MaxInputTokens < c.Retrieval.ChunkTokens {
		return InvalidEnv("EMBED_MAX_TOKENS", "must be at least CHUNK_TOKENS or chunks get silently truncated")
	}

	if c.Retrieval.TopK < 1 {
		return InvalidEnv("RETRIEVAL_TOP_K", "must be at least 1")
	}

	if c.Retrieval.MinSimilarity < -1 || c.Retrieval.MinSimilarity > 1 {
		return InvalidEnv("RETRIEVAL_MIN_SIMILARITY", "must be between -1 and 1")
	}

	if c.Retrieval.IndexBatchSize < 1 {
		return InvalidEnv("INDEX_BATCH_SIZE", "must be at least 1")
	}

	if c.Retrieval.IndexInterval <= 0 {
		return InvalidEnv("INDEX_INTERVAL", "must be positive")
	}

	// A lease shorter than an embedding call would have two workers index the same
	// note at once, which is exactly the double spend the lease exists to prevent. The
	// bound is on the interval rather than on a stopwatch: a lease below the polling
	// interval cannot be used to hide anything.
	if c.Retrieval.IndexLease <= c.Retrieval.IndexInterval {
		return InvalidEnv("INDEX_LEASE", "must be longer than INDEX_INTERVAL")
	}

	if c.Telemetry.Enabled && c.Telemetry.SampleRatio <= 0 {
		return InvalidEnv("OTEL_TRACES_SAMPLER_ARG", "must be greater than 0 when an OTLP endpoint is set")
	}

	return nil
}

func getEnv(key string, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}

	return fallback
}

// decodeOAuthKey reads a connector encryption key from hex or base64.
//
// Both encodings are accepted because both are what people have to hand: hex from
// `openssl rand -hex 32`, base64 from `openssl rand -base64 32`. Anything that does not
// decode to exactly the required length yields no key, and Load turns that into a
// configuration error rather than letting the cipher be built with something short.
//
// Base64 is tried second because a 64-character hex string is also valid base64 and
// would decode to the wrong number of bytes. The length check is what disambiguates:
// a base64 encoding of 32 bytes is 44 characters, so the two cannot both match.
func decodeOAuthKey(raw string) []byte {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}

	if decoded, err := hex.DecodeString(trimmed); err == nil && len(decoded) == OAuthEncryptionKeyBytes {
		return decoded
	}

	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(trimmed); err == nil && len(decoded) == OAuthEncryptionKeyBytes {
			return decoded
		}
	}

	return nil
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))

	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}

	return out
}

func getEnvBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(getEnv(key, ""))
	if raw == "" {
		return fallback
	}

	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}

	return value
}

func getEnvInt(key string, fallback int) int {
	raw := strings.TrimSpace(getEnv(key, ""))
	if raw == "" {
		return fallback
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}

	return value
}

func getEnvFloat(key string, fallback float64) float64 {
	raw := strings.TrimSpace(getEnv(key, ""))
	if raw == "" {
		return fallback
	}

	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}

	return value
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	d, err := getEnvDurationErr(key, fallback)
	if err != nil {
		return fallback
	}

	return d
}

func getEnvDurationErr(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(getEnv(key, ""))
	if raw == "" {
		return fallback, nil
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %w", ErrInvalidEnv, key, err)
	}

	return d, nil
}

// ErrMissingEnv marks a required variable that was not provided.
func ErrMissingEnv(key string) error {
	return &MissingEnvError{Key: key}
}

type MissingEnvError struct {
	Key string
}

func (e *MissingEnvError) Error() string {
	return "required environment variable not set: " + e.Key
}

// ErrInvalidEnv marks a variable that was set to an unusable value.
var ErrInvalidEnv = fmt.Errorf("invalid environment configuration")

// InvalidEnv reports a variable whose value cannot be used.
func InvalidEnv(key string, reason string) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalidEnv, key, reason)
}

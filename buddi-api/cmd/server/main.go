package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	gormlogger "gorm.io/gorm/logger"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/api"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/agent"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/auth"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/calendar"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/chat"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/note"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/oauth"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/retrieval"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/task"
	"github.com/akinolaemmanuel49/buddi-api/internal/config"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/googlecalendar"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/googleoauth"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/llm/ollama"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/mcp"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence/gormdb"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence/migrations"
	"github.com/akinolaemmanuel49/buddi-api/internal/observability"
	"github.com/akinolaemmanuel49/buddi-api/internal/server"
)

// version is overridable at build time with
// -ldflags "-X main.version=$(git describe --tags)".
var version = "dev"

func main() {
	if err := run(); err != nil {
		// The configured logger may not exist yet, so report through the default.
		slog.Error("server exited", "error", err)
		os.Exit(1)
	}
}

// oauthTokenSource resolves a user's live access token for a connector.
//
// It exists so the connector never sees the store or the cipher. The service decides
// when a token needs refreshing; the connector only ever receives a token that is
// currently valid, and receives it per call so it is not held between requests.
type oauthTokenSource struct {
	service *oauth.Service
}

func (s oauthTokenSource) AccessToken(ctx context.Context, userID string) (string, error) {
	parsed, err := uuid.Parse(userID)
	if err != nil {
		return "", fmt.Errorf("connector asked for a token for a user id that is not one")
	}

	token, err := s.service.AccessToken(ctx, parsed, domain.ProviderGoogleCalendar)
	if err != nil {
		return "", err
	}

	return token, nil
}

// calendarRegistryTool adapts the calendar connector's write tool to the agent's Tool
// contract.
//
// The tools are discovered over a real MCP handshake rather than read from the
// connector's own list, so what gets registered is what a client would actually see.
func calendarRegistryTool(ctx context.Context, resolver *agent.PipeSessionResolver) (agent.Tool, error) {
	// Discovery needs a user, and registration happens once at startup before any user
	// has connected. The tools are the same for everybody — only the credential behind
	// them differs — so any id describes the same tool list. A session opened here is
	// closed again rather than cached against that id.
	// The tools are the same for everybody — only the credential behind them
	// differs — so the nil id describes the same tool list a user would see.
	discoveryUser := uuid.UUID{}

	descriptors, err := agent.ToolsFor(resolver, ctx, discoveryUser, func(name string) bool {
		return name == calendar.ListEventsTool
	})
	if err != nil {
		return nil, err
	}

	var found agent.Tool

	for _, descriptor := range descriptors {
		if descriptor.Name != calendar.CreateEventTool {
			continue
		}

		tool, err := agent.NewMCPTool(agent.MCPToolOptions{
			Server:   resolver.Name(),
			Tool:     descriptor,
			Resolver: resolver,
		})
		if err != nil {
			return nil, err
		}

		found = tool
	}

	if found == nil {
		return nil, fmt.Errorf("the calendar connector does not offer %s", calendar.CreateEventTool)
	}

	return found, nil
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log, shutdownTelemetry, err := observability.Setup(ctx, observability.Options{
		ServiceName: cfg.Telemetry.ServiceName,
		ServiceVer:  version,
		Environment: cfg.Environment,
		Endpoint:    cfg.Telemetry.Endpoint,
		SampleRatio: cfg.Telemetry.SampleRatio,
		Insecure:    cfg.Telemetry.Insecure,
		LogLevel:    cfg.LogLevel,
	})
	if err != nil {
		return err
	}

	defer func() {
		flushCtx, cancelFlush := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelFlush()

		if err := shutdownTelemetry(flushCtx); err != nil {
			log.Error("telemetry shutdown failed", "error", err)
		}
	}()

	log.Info("starting", "version", version, "environment", cfg.Environment,
		"model", cfg.Ollama.Model, "embed_model", cfg.Ollama.EmbedModel,
		"telemetry", cfg.Telemetry.Enabled)

	// Migrations run before the pool opens so the rest of startup sees a schema
	// it can rely on, and so a schema problem fails fast with a clear message.
	if cfg.Database.MigrateOnStart {
		result, err := migrations.Up(ctx, cfg.Database.DSN)
		if err != nil {
			return err
		}

		if len(result.Applied) > 0 {
			log.Info("migrations applied", "files", result.Applied, "version", result.Version.Number)
		} else {
			log.Info("schema up to date", "version", result.Version.Number)
		}
	}

	db := gormdb.New(gormdb.Config{
		DSN:              cfg.Database.DSN,
		MaxOpenConns:     cfg.Database.MaxOpenConns,
		MaxIdleConns:     cfg.Database.MaxIdleConns,
		ConnMaxLifetime:  cfg.Database.ConnMaxLifetime,
		ConnMaxIdleTime:  cfg.Database.ConnMaxIdleTime,
		LogLevel:         gormLogLevel(cfg.LogLevel),
		LogSlowThreshold: cfg.Database.SlowQueryAfter,
	})

	connectCtx, cancelConnect := context.WithTimeout(ctx, 10*time.Second)
	defer cancelConnect()

	if err := db.Connect(connectCtx); err != nil {
		return err
	}

	defer func() {
		if err := db.Close(); err != nil {
			log.Error("closing database failed", "error", err)
		}
	}()

	log.Info("database connected")

	// The model runtime is validated and probed at startup but deliberately not
	// required. Auth, notes and tasks do not depend on it, so an unstarted Ollama
	// must not stop the API from serving requests. A malformed base URL is still
	// fatal, because that is a configuration mistake rather than a runtime being
	// down, and a silent one that would otherwise surface much later.
	llm, err := ollama.New(cfg.Ollama.BaseURL, ollama.Options{
		Model:         cfg.Ollama.Model,
		ContextLength: cfg.Ollama.ContextLength,
		Temperature:   cfg.Ollama.Temperature,
		KeepAlive:     cfg.Ollama.KeepAlive,
		Timeout:       cfg.Ollama.Timeout,
	})
	if err != nil {
		return err
	}

	probeCtx, cancelProbe := context.WithTimeout(ctx, 10*time.Second)
	defer cancelProbe()

	version, probeErr := llm.Version(probeCtx)
	if probeErr != nil {
		log.Warn("model runtime unreachable, agent features will fail until it is up",
			"base_url", cfg.Ollama.BaseURL, "error", probeErr)
	} else {
		log.Info("model runtime reachable", "base_url", cfg.Ollama.BaseURL,
			"version", version, "model", cfg.Ollama.Model, "planner_model", cfg.Ollama.PlannerModelOrDefault())

		// A reachable runtime with the wrong models in it is still an outage for
		// the agent surface, and it is the common case after a fresh pull. Warn
		// with the exact command rather than letting the first real request fail.
		warnMissingModels(ctx, log, llm, cfg.Ollama)

		warmPlannerModel(ctx, log, llm, cfg.Ollama)
	}

	// Repositories are built once and shared. They hold no per-request state, so
	// a single instance serves every request; the transaction that a call
	// belongs to travels on the context, not on the repository.
	users := gormdb.NewUserRepository(db.Conn())
	refreshTokens := gormdb.NewRefreshTokenRepository(db.Conn())
	notes := gormdb.NewNoteRepository(db.Conn())
	tasks := gormdb.NewTaskRepository(db.Conn())
	runs := gormdb.NewAgentRepository(db.Conn())
	chunks := gormdb.NewNoteChunkRepository(db.Conn())

	taskService := task.NewService(tasks, nil)

	retrievalService, err := retrieval.NewService(
		chunks,
		ollama.NewEmbedder(llm).
			WithModel(cfg.Ollama.EmbedModel).
			WithDimensions(cfg.Retrieval.EmbeddingDimensions),
		retrieval.Options{
			Model:         cfg.Ollama.EmbedModel,
			Dimensions:    cfg.Retrieval.EmbeddingDimensions,
			TopK:          cfg.Retrieval.TopK,
			MinSimilarity: cfg.Retrieval.MinSimilarity,
			Bounds:        retrieval.DefaultChunkBounds(cfg.Retrieval.ChunkTokens, cfg.Retrieval.ChunkOverlap, cfg.Retrieval.MaxInputTokens),
		},
	)
	if err != nil {
		return err
	}

	// Indexing is best effort. The note write has already succeeded by the time it
	// runs, so the failure is logged rather than reported as a rejected request:
	// telling the caller their note was lost when it was stored would be worse than
	// a note that is not searchable until it is next edited.
	noteService := note.NewService(notes, nil).
		WithIndexer(logIndexer{indexer: retrievalService, log: log}).
		WithSearchStateObserver(logSearchStateObserver{log: log})

	// The worker owns the retries, so an embedding runtime that is briefly down is
	// repaired without the user editing the note again. It runs alongside the inline
	// indexer rather than instead of it: the inline path makes a saved note
	// searchable immediately, and the worker covers everything it could not finish.
	indexWorker, err := note.NewWorker(notes, retrievalService, note.WorkerOptions{
		BatchSize: cfg.Retrieval.IndexBatchSize,
		Interval:  cfg.Retrieval.IndexInterval,
		Lease:     cfg.Retrieval.IndexLease,
	})
	if err != nil {
		return err
	}

	indexWorker.WithLogger(log).WithSearchStateObserver(logSearchStateObserver{log: log})

	planService, err := planner.NewService(
		// The think level is configurable because the planner model is. A reasoning
		// model wants a cheap level, since the runtime rejects think:false alongside a
		// schema; a non-thinking instruct model wants no think field at all, since
		// asking it to reason at a level it does not implement spends the token budget
		// on nothing. Setting BUDDI_PLANNER_THINK to empty gives the latter.
		ollama.NewAdapter(llm).
			WithModel(cfg.Ollama.PlannerModelOrDefault()).
			WithThinkLevel(cfg.Ollama.PlannerThink),
		planner.Options{
			Model:       cfg.Ollama.PlannerModelOrDefault(),
			MaxRetries:  cfg.Ollama.PlanRetries,
			ContextTopK: cfg.Retrieval.TopK,
		},
	)
	if err != nil {
		return err
	}

	planService.WithRetriever(chunkRetriever{service: retrievalService, log: log})

	// The registry decides which tool carries out a plan's intent. Task creation is the
	// default, and it is the only binding a deployment without an encryption key gets:
	// connectors register themselves below.
	registryOptions := []agent.RegistryOption{agent.TaskCreateBinding(taskService)}

	var oauthService *oauth.Service
	var calendarTools *calendar.Tools
	var calendarResolver *agent.PipeSessionResolver

	// googleProviders is declared out here because the oauth service needs the
	// provider to refresh tokens, and the provider only exists if its application was
	// configured.
	var googleProviders []oauth.Provider
	var googleProvider *googleoauth.Provider
	var stateSigner *googleoauth.Signer
	var calendarToolRegistered bool

	if cfg.OAuth.OAuthEnabled() {
		cipher, err := oauth.NewAESCipher(cfg.OAuth.EncryptionKey)
		if err != nil {
			return err
		}

		googleConfig := googleoauth.Config{
			ClientID:     cfg.OAuth.GoogleClientID,
			ClientSecret: cfg.OAuth.GoogleClientSecret,
			RedirectURL:  cfg.OAuth.GoogleRedirectURL,
		}

		// The connector is only usable once the OAuth application exists. Registering
		// it anyway would produce a tool that fails every call with an opaque message,
		// so a deployment missing any of the three values is told exactly which.
		// Assigned with = rather than :=. Both variables already exist in the function
		// scope, so := here would declare a second, shadowing googleProvider that died
		// with this block.
		googleProvider, err = googleoauth.New(googleConfig)
		if err != nil {
			log.Warn("google calendar connector not registered: its OAuth application is not configured",
				"missing", missingGoogleConfig(googleConfig),
				"hint", "set GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET and GOOGLE_REDIRECT_URL")

			googleProvider = nil
		} else {
			googleProviders = append(googleProviders, googleProvider)

			// The state is signed with the signing secret, which the config already
			// requires to be at least 32 bytes, so there is no second secret to
			// configure and therefore no second one to rotate or leak.
			stateSigner, err = googleoauth.NewSigner(cfg.Auth.AccessSecret)
			if err != nil {
				return err
			}
		}

		// Built after the provider, never before. The provider list is what lets the
		// service renew a stored access token, and the slice is captured by value at
		// construction: a service built with an empty list stores and hands back tokens
		// perfectly and then fails at the first refresh with "no refresh implementation",
		// which reads as a Google outage rather than as a wiring mistake.
		oauthService, err = oauth.NewService(
			gormdb.NewOAuthTokenRepository(db.Conn()),
			cipher,
			googleProviders,
			oauth.Options{RefreshMargin: cfg.OAuth.RefreshMargin},
		)
		if err != nil {
			return err
		}

		// Asserted here rather than left to fail in use: the provider list is captured
		// when the service is built, so a service that cannot renew this provider would
		// work right up until a user's access token expired and then fail with an error
		// naming Google rather than the wiring.
		if googleProvider != nil && !oauthService.HasProvider(googleoauth.ProviderName) {
			log.Warn("the oauth service cannot refresh google credentials",
				"fix", "the provider must be registered before the service is constructed")
		}

		calendarClient, err := googlecalendar.New(googlecalendar.Options{})
		if err != nil {
			return err
		}

		// The connector resolves a token per call through the OAuth store, so a
		// credential is read at execution time and never held in the tool's arguments.
		calendarTools = calendar.NewTools(calendarClient, oauthTokenSource{service: oauthService})

		calendarResolver, err = agent.NewPipeSessionResolver(
			"calendar",
			func(_ context.Context, userID uuid.UUID) ([]mcp.ToolHandler, error) {
				return calendar.MCPHandlers(userID.String(), calendarTools), nil
			},
		)
		if err != nil {
			return err
		}

		calendarTool, err := calendarRegistryTool(ctx, calendarResolver)
		if err != nil {
			return err
		}

		registryOptions = append(registryOptions, agent.CalendarBinding(calendarTool))

		calendarToolRegistered = true

		log.Info("calendar connector registered",
			"google_configured", googleProvider != nil,
			"refresh_margin", cfg.OAuth.RefreshMargin,
		)
	} else {
		log.Info("no connector encryption key configured; third-party connectors are off")
	}

	toolRegistry, err := agent.NewRegistry(registryOptions...)
	if err != nil {
		return err
	}

	agentService, err := agent.NewService(runs, planService, toolRegistry, agent.Options{
		// Planning and execution are both answered synchronously, so these have
		// to stay under the server's write timeout or the response is abandoned
		// before the work is recorded.
		PlanTimeout: cfg.Agent.PlanTimeout,
		ToolTimeout: cfg.MCP.ToolTimeout,
	})
	if err != nil {
		return err
	}

	// Chat reuses the planner for turns that turn out to be tasks, and a separate
	// streaming adapter for the ones that are conversation. The chat adapter asks for
	// no reasoning level by default: a reply is prose, and on a CPU runtime tokens
	// spent reasoning are tokens not spent answering.
	//
	// It is built after the agent service because it records the runs that a planned
	// turn produces, and needs the tool registry that service closes over.
	chatService, err := chat.NewService(
		gormdb.NewConversationRepository(db.Conn()),
		gormdb.NewMessageRepository(db.Conn()),
		planService,
		runRecorder{agent: agentService},
		ollama.NewChatAdapter(llm).WithModel(cfg.Ollama.Model),
		chat.Options{},
	)
	if err != nil {
		return err
	}

	authService, err := auth.NewService(
		users,
		refreshTokens,
		auth.NewBcryptHasher(cfg.Auth.BcryptCost),
		auth.NewJWTIssuer(cfg.Auth.AccessSecret, cfg.Auth.Issuer),
		auth.Options{
			AccessTTL:  cfg.Auth.AccessTTL,
			RefreshTTL: cfg.Auth.RefreshTTL,
			Tx:         db,
		},
	)
	if err != nil {
		return err
	}

	handlerOpts := api.Options{
		Logger:         log,
		Database:       db,
		AllowedOrigins: cfg.AllowedOrigins,
		WebOrigin:      cfg.WebOrigin,
		Auth:           authService,
		Notes:          noteService,
		Tasks:          taskService,
		Agent:          agentService,
		Chat:           chatService,
	}

	// Assigned conditionally rather than inline, because a nil *Provider stored in an
	// interface is not a nil interface: the handlers' `== nil` check would pass and
	// every call would reach a nil receiver. The connectors are optional, so this has
	// to be explicit rather than relying on the zero value.
	if googleProvider != nil {
		handlerOpts.Authorizer = googleProvider
		handlerOpts.Exchanger = googleProvider
		handlerOpts.States = stateSigner
	}

	if oauthService != nil {
		handlerOpts.Connections = oauthService
	}

	handler := api.New(handlerOpts).Handler()

	// The write tool and the connection endpoints are wired from the same provider
	// variable, so they should agree. They previously did not, and the disagreement
	// presented as "this deployment has no Google credentials configured" on a
	// deployment that had all three, which sent the reader looking in the wrong place.
	if calendarToolRegistered && handlerOpts.Authorizer == nil {
		log.Warn("the calendar write tool is registered but the connection endpoints have no authorizer",
			"fix", "the provider is probably being shadowed when it is constructed")
	}

	addr := ":" + cfg.Port

	srv := server.NewHTTPServerWithOptions(addr, observability.Instrument(handler, cfg.Telemetry.ServiceName), server.Options{
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Writes are bounded by the per-request budget, so this only needs to
		// exceed it plus room for the final flush.
		WriteTimeout: cfg.RequestTimeout + 10*time.Second,
		IdleTimeout:  60 * time.Second,
	})

	serveErr := make(chan error, 1)

	go func() {
		log.Info("http server listening", "addr", addr)
		serveErr <- srv.Start()
	}()

	// The worker gets its own context so shutdown can stop it and wait for it. It is
	// stopped before the deferred database close rather than alongside it: a worker
	// holding a query on a pool that has been closed reports failures nobody is left
	// to read, and the notes it was holding stay leased until they expire.
	workerCtx, stopWorker := context.WithCancel(ctx)

	workerDone := make(chan struct{})

	go func() {
		defer close(workerDone)

		log.Info("indexing worker started",
			"batch_size", cfg.Retrieval.IndexBatchSize,
			"interval", cfg.Retrieval.IndexInterval,
			"lease", cfg.Retrieval.IndexLease,
		)

		indexWorker.Run(workerCtx)

		log.Info("indexing worker stopped")
	}()

	defer func() {
		stopWorker()

		// Bounded by the shutdown grace period rather than waited on: the worker may
		// be mid-embedding, and holding the process open for a best-effort index would
		// turn a slow model into a slow shutdown.
		select {
		case <-workerDone:
		case <-time.After(cfg.ShutdownGrace):
			log.Warn("indexing worker did not stop within the shutdown grace period")
		}
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}

		return nil

	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}

	log.Info("shutdown complete")

	return nil
}

// chunkRetriever adapts the retrieval service to the planner's port.
//
// The adapter exists so the planner does not import the retrieval package: the
// planner declares the narrow shape it wants and this is the one place that knows
// both. It also logs, because the planner treats a retrieval failure as grounds for
// planning without context and would otherwise report an ungrounded plan as if it
// had been grounded.
type chunkRetriever struct {
	service *retrieval.Service
	log     *slog.Logger
}

func (c chunkRetriever) Search(
	ctx context.Context,
	userID uuid.UUID,
	query string,
	topK int,
) ([]planner.ContextChunk, error) {
	hits, err := c.service.Search(ctx, userID, query, topK)
	if err != nil {
		return nil, err
	}

	chunks := make([]planner.ContextChunk, 0, len(hits))

	for _, hit := range hits {
		chunks = append(chunks, planner.ContextChunk{
			NoteID:  hit.NoteID,
			Ordinal: hit.Ordinal,
			Content: hit.Content,
		})
	}

	if len(chunks) > 0 {
		c.log.Debug("grounded plan in retrieved notes",
			"user_id", userID,
			"chunks", len(chunks),
		)
	}

	return chunks, nil
}

// logIndexer makes a failed indexing attempt visible without letting it fail the
// note write it follows.
//
// The note service drops the error on purpose, so without this wrapper an
// embedding runtime that is down would be invisible: notes would save normally
// and simply stop appearing in search results.
type logIndexer struct {
	indexer note.Indexer
	log     *slog.Logger
}

func (l logIndexer) Index(ctx context.Context, n *domain.Note) error {
	err := l.indexer.Index(ctx, n)
	if err != nil {
		l.log.Warn("could not index note for search",
			"note_id", n.ID,
			"error", err,
		)
	}

	return err
}

// logSearchStateObserver reports a failure to record the outcome of an indexing
// attempt.
//
// Logged rather than returned because the note was written successfully before any of
// this ran, and failing the request now would tell the user their note was lost when it
// is safely stored. It matters more than it looks: without it the state of a note in
// storage is whatever the last successful write left behind, and the note the API
// returns still claims to be indexed.
type logSearchStateObserver struct {
	log *slog.Logger
}

func (l logSearchStateObserver) ObserveSearchStateError(_ context.Context, n *domain.Note, err error) {
	l.log.Error("could not record note search state",
		"note_id", n.ID,
		"user_id", n.UserID,
		"search_state", n.SearchState,
		"index_attempts", n.IndexAttempts,
		"error", err,
	)
}

// warnMissingModels reports configured models the runtime does not have. It only
// warns: the API is fully usable without any model, and refusing to start would
// make an unrelated endpoint unavailable because of a missing pull.
func warnMissingModels(ctx context.Context, log *slog.Logger, llm *ollama.Client, cfg config.Ollama) {
	models, err := llm.Tags(ctx)
	if err != nil {
		log.Warn("could not list model runtime contents", "error", err)

		return
	}

	available := make(map[string]bool, len(models))
	for _, m := range models {
		// The runtime reports an implicit tag, so a model pulled as
		// "nomic-embed-text" comes back as "nomic-embed-text:latest".
		available[strings.TrimSuffix(m.Name, ":latest")] = true
	}

	// The planner usually falls back to the general model, so the same missing
	// model can appear twice in this list. Warn once per distinct name.
	checked := map[string]bool{}

	for _, wanted := range []string{cfg.Model, cfg.PlannerModelOrDefault(), cfg.EmbedModel} {
		name := strings.TrimSuffix(wanted, ":latest")

		if wanted == "" || checked[name] || available[name] {
			continue
		}

		checked[name] = true

		log.Warn("configured model is not present in the runtime",
			"model", wanted, "fix", "docker exec buddi-ollama ollama pull "+wanted)
	}
}

// warmPlannerModel loads the planning model into memory at start-up.
//
// It exists because the cost of paging several gigabytes of weights in from disk is
// charged to whichever request happens to arrive first. Measured on a CPU-only
// runtime, that load took 74s for a 4B model against a 45s planning timeout, so
// without this the first request after every idle period failed on a cost that has
// nothing to do with the request.
//
// It is best effort. A runtime that cannot warm is still usable: the first request
// just pays the load itself, which is the behaviour this is improving on rather
// than one it introduces.
func warmPlannerModel(ctx context.Context, log *slog.Logger, llm *ollama.Client, cfg config.Ollama) {
	model := cfg.PlannerModelOrDefault()

	// Bounded generously because this is a one-off cost, not request latency, and
	// it must not be cut short by the planning timeout it exists to avoid.
	warmCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	start := time.Now()

	_, err := llm.Generate(warmCtx, ollama.GenerateRequest{
		Model:      model,
		Prompt:     "Reply with the single word: ready.",
		NumPredict: 8,
		// Left unset rather than inheriting the planner's think level: the reply is
		// discarded, so a model that spends tokens reasoning first would only make
		// warming slower.
	})
	if err != nil {
		log.Warn("could not pre-load the planning model; the first plan will be slower",
			"model", model, "error", err)

		return
	}

	log.Info("planning model resident", "model", model,
		"took", time.Since(start).Round(time.Millisecond).String())
}

// missingGoogleConfig names the OAuth values that are absent.
//
// Named rather than reported as a single "not configured", because the three are set
// in three different places in the Google console and knowing which one is missing
// is the difference between a two-minute fix and an afternoon.
func missingGoogleConfig(config googleoauth.Config) []string {
	var missing []string

	if config.ClientID == "" {
		missing = append(missing, "GOOGLE_CLIENT_ID")
	}

	if config.ClientSecret == "" {
		missing = append(missing, "GOOGLE_CLIENT_SECRET")
	}

	if config.RedirectURL == "" {
		missing = append(missing, "GOOGLE_REDIRECT_URL")
	}

	return missing
}

// runRecorder adapts the agent service to the chat layer's port.
//
// It exists because the two packages disagree about the shape of a run, and neither
// should import the other to find out: the agent layer returns its own Run with
// approvals and results attached, while chat only needs an id and a status.
type runRecorder struct {
	agent *agent.Service
}

func (r runRecorder) PlanFromOutcome(
	ctx context.Context,
	userID uuid.UUID,
	goal string,
	outcome *planner.Outcome,
) (chat.RunRecord, error) {
	run, err := r.agent.PlanFromOutcome(ctx, userID, goal, outcome)
	if err != nil {
		return chat.RunRecord{}, err
	}

	return chat.RunRecord{ID: run.Run.ID, Status: string(run.Run.Status)}, nil
}

func gormLogLevel(level string) gormlogger.LogLevel {
	switch strings.ToLower(level) {
	case "debug":
		return gormlogger.Info
	case "warn", "warning":
		return gormlogger.Warn
	case "error":
		return gormlogger.Error
	case "silent":
		return gormlogger.Silent
	default:
		return gormlogger.Warn
	}
}

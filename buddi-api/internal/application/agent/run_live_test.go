package agent_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/agent"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence/gormdb"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence/migrations"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// The agent service's own tests use an in-memory store, which stores the pointer it
// was handed. That makes them blind to the one thing a real store decides: which
// columns the update statement actually writes.
//
// A run is inserted before it is planned and updated once planning finishes, so a
// column the update forgets keeps the default it was inserted with. grounding_state
// and plan_fallback were both forgotten that way, and every run reported itself
// ungrounded while its plan was written from the user's own notes. The plan looked
// right, which is exactly why nothing else caught it.
//
//	go test ./internal/application/agent with
//	BUDDI_TEST_DATABASE_URI=postgres://buddi:buddi@localhost:5432/buddi_test?sslmode=disable
func TestLiveRunKeepsItsGroundingStateAndFallbackFlag(t *testing.T) {
	tests := map[string]struct {
		grounding domain.GroundingState
		fallback  bool
	}{
		"notes reached the planner": {grounding: domain.GroundingGrounded, fallback: false},
		"retrieval matched nothing": {grounding: domain.GroundingNoContext, fallback: false},
		"retrieval failed":          {grounding: domain.GroundingUngrounded, fallback: false},
		// The fallback is the one case that would have been reported as a model
		// plan, because false is also the column default.
		"the model could not be used": {grounding: domain.GroundingNoContext, fallback: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			db := liveDatabase(t)
			user := liveUser(t, db)
			store := gormdb.NewAgentRepository(db)

			service := liveService(t, store, &fixedPlanner{outcome: &planner.Outcome{
				Plan:         &planner.Plan{Intent: domain.PlanIntentTask, Title: "Freeze merges"},
				UsedFallback: tt.fallback,
				Grounding:    tt.grounding,
			}})

			planned, err := service.Plan(t.Context(), user.ID, "release the billing service")
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}

			// The in-memory run is the service's own copy, so it agrees with the
			// planner by construction. Only a read back from the database can tell
			// whether the value survived the write.
			stored, err := store.GetRun(t.Context(), user.ID, planned.Run.ID)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}

			if stored.GroundingState != tt.grounding {
				t.Errorf("stored GroundingState = %q, want %q", stored.GroundingState, tt.grounding)
			}

			if stored.PlanFallback != tt.fallback {
				t.Errorf("stored PlanFallback = %v, want %v", stored.PlanFallback, tt.fallback)
			}
		})
	}
}

// TestLiveRunIsNotWrittenAcrossOwners covers the other half of the update: the
// statement is scoped to the owner, so a second user cannot move it and cannot read
// it either.
func TestLiveRunIsNotWrittenAcrossOwners(t *testing.T) {
	db := liveDatabase(t)
	owner := liveUser(t, db)
	other := liveUser(t, db)
	store := gormdb.NewAgentRepository(db)

	service := liveService(t, store, &fixedPlanner{outcome: &planner.Outcome{
		Plan:      &planner.Plan{Intent: domain.PlanIntentTask, Title: "Freeze merges"},
		Grounding: domain.GroundingGrounded,
	}})

	planned, err := service.Plan(t.Context(), owner.ID, "release the billing service")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	run := planned.Run
	run.Status = domain.RunStatusCompleted

	if err := store.UpdateRun(t.Context(), run); err != nil {
		t.Fatalf("UpdateRun as owner: %v", err)
	}

	// The same object with another user's id must not match the row.
	run.UserID = other.ID

	if err := store.UpdateRun(t.Context(), run); err == nil {
		t.Fatal("UpdateRun as another user succeeded, want a not-found error")
	}

	if _, err := store.GetRun(t.Context(), other.ID, planned.Run.ID); err == nil {
		t.Fatal("GetRun as another user succeeded, want a not-found error")
	}
}

// fixedPlanner returns one outcome, which is all a persistence test needs: the model
// is not what is under test.
type fixedPlanner struct {
	outcome *planner.Outcome
	err     error
}

func (p *fixedPlanner) Plan(_ context.Context, _ uuid.UUID, _ string) (*planner.Outcome, error) {
	if p.err != nil {
		return nil, p.err
	}

	return p.outcome, nil
}

// noopTool is registered so a proposed plan has a tool to attach its approval to.
// Nothing executes it: these tests stop at awaiting approval.
type noopTool struct{}

func (noopTool) Name() string        { return "tasks.create" }
func (noopTool) Description() string { return "creates a task" }
func (noopTool) Mutating() bool      { return true }

func (noopTool) Validate(json.RawMessage) error { return nil }

func (noopTool) Execute(
	context.Context,
	uuid.UUID,
	uuid.UUID,
	json.RawMessage,
) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

// liveService wires the real service over the no-op tool, which is all a
// persistence test needs: nothing here executes a tool.
func liveService(t *testing.T, store agent.RunStore, p agent.Planner) *agent.Service {
	t.Helper()

	registry, err := agent.NewRegistry(agent.RegistryOption{
		Intent:  domain.PlanIntentTask,
		Tool:    noopTool{},
		Encode:  noopEncode,
		Default: true,
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	service, err := agent.NewService(store, p, registry, agent.Options{Clock: time.Now})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	return service
}

// noopEncode renders the plan as an empty payload: the tool accepts anything.
func noopEncode(*planner.Plan) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }

func liveDatabase(t *testing.T) *gorm.DB {
	t.Helper()

	dsn := liveTestDSN(t)

	if _, err := migrations.Up(t.Context(), dsn); err != nil {
		t.Fatalf("migrate %s: %v", dsn, err)
	}

	db := gormdb.New(gormdb.DefaultConfig(dsn))
	if err := db.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	conn := db.Conn()

	// Runs cascade away with their users, but clearing them keeps the assertions
	// independent of what ran before.
	if err := conn.Exec("DELETE FROM agent_runs").Error; err != nil {
		t.Fatalf("clear runs: %v", err)
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

func liveTestDSN(t *testing.T) string {
	t.Helper()

	configured := os.Getenv("BUDDI_TEST_DATABASE_URI")
	if configured == "" {
		t.Skip("set BUDDI_TEST_DATABASE_URI to a disposable database to run this test")
	}

	parsed, err := url.Parse(configured)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}

	// A separate database from the note tests: it deletes rows other packages read,
	// and sharing one would make the suites order-dependent.
	name := strings.TrimPrefix(parsed.Path, "/")
	if name == "" || name == "buddi" {
		t.Fatalf("refusing to run against %q, which is not a disposable database", name)
	}

	name = strings.TrimSuffix(name, "_agentrun") + "_agentrun"
	parsed.Path = "/" + name

	liveCreateDatabase(t, configured, name)

	return parsed.String()
}

func liveCreateDatabase(t *testing.T, configured, name string) {
	t.Helper()

	maintenance, err := url.Parse(configured)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}

	maintenance.Path = "/postgres"

	pool, err := sql.Open("pgx", maintenance.String())
	if err != nil {
		t.Fatalf("open maintenance database: %v", err)
	}

	defer func() { _ = pool.Close() }()

	// "already exists" is the expected outcome on every run after the first and is not
	// worth reporting.
	if _, err := pool.ExecContext(t.Context(), fmt.Sprintf("CREATE DATABASE %s", name)); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("create %s: %v", name, err)
	}
}

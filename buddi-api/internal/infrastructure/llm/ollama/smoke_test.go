package ollama_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/llm/ollama"
)

// This file is the Phase 3.5 evaluation harness. It talks to a real runtime, so
// it is skipped unless BUDDI_SMOKE_OLLAMA=1 is set explicitly:
//
//	BUDDI_SMOKE_OLLAMA=1 go test ./internal/infrastructure/llm/ollama/ -run Smoke -v
//
// It exists to answer questions that unit tests cannot: which models are worth
// keeping, whether planner-grade structured output survives a CPU-only runtime,
// and what the latency actually costs. It reports numbers rather than asserting
// thresholds, because a developer's machine is not a benchmark and a hard
// assertion here would just produce flaky failures that get ignored.
const smokeEnabledEnv = "BUDDI_SMOKE_OLLAMA"

func smokeClient(t *testing.T) *ollama.Client {
	t.Helper()

	if os.Getenv(smokeEnabledEnv) != "1" {
		t.Skipf("set %s=1 to run against a live Ollama runtime", smokeEnabledEnv)
	}

	base := os.Getenv("BUDDI_OLLAMA_BASE_URL")
	if base == "" {
		base = "http://127.0.0.1:11434"
	}

	client, err := ollama.New(base, ollama.Options{
		ContextLength: 8192,
		Temperature:   0.2,
		KeepAlive:     2 * time.Minute,
		Timeout:       10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return client
}

// smokeModel is the model the harness exercises. It defaults to the only
// general-purpose model the application uses, so the default and production
// configuration cannot drift apart unnoticed.
func smokeModel(t *testing.T) string {
	t.Helper()

	if model := os.Getenv("BUDDI_SMOKE_MODEL"); model != "" {
		return model
	}

	return "qwen3:0.6b"
}

// plannerSchema is the shape the planner needs: a small, flat object with an
// enum and a list, which is representative without being trivial.
var plannerSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "title": {"type": "string"},
    "priority": {"type": "string", "enum": ["low", "medium", "high"]},
    "due_at": {"type": "string"},
    "steps": {"type": "array", "items": {"type": "string"}}
  },
  "required": ["title", "priority", "steps"]
}`)

func TestSmokeRuntimeIsReachable(t *testing.T) {
	client := smokeClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	version, err := client.Version(ctx)
	if err != nil {
		t.Fatalf("runtime unreachable at the configured base url: %v", err)
	}

	t.Logf("runtime version: %s", version)

	models, err := client.Tags(ctx)
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}

	if len(models) == 0 {
		t.Fatal("no models in the runtime; pull at least one with `ollama pull <model>`")
	}

	for _, m := range models {
		t.Logf("model available: %-24s %.2f GB", m.Name, float64(m.SizeBytes)/(1<<30))
	}
}

// TestSmokeGeneration checks that the configured model can actually complete a
// request, and reports what it costs.
//
// This used to compare several model tiers. Only qwen3:0.6b is in use now, so
// there is nothing left to compare: the question is whether the one model is
// healthy and how fast it is.
func TestSmokeGeneration(t *testing.T) {
	client := smokeClient(t)

	model := smokeModel(t)
	think := false

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	start := time.Now()

	res, err := client.Generate(ctx, ollama.GenerateRequest{
		Model:      model,
		Prompt:     "Reply with exactly one word: the capital of France.",
		NumPredict: 64,
		Think:      &think,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	answer := strings.TrimSpace(res.Response)

	t.Logf("%-14s %6.2fs wall, %5.1f tok/s decode, %d tokens, answer=%q",
		model, time.Since(start).Seconds(), res.TokensPerSecond(), res.EvalCount, answer)

	if answer == "" {
		t.Error("the model returned nothing; on a thinking model this usually means NumPredict was too small to get past the reasoning")
	}

	// The resident size is the number that decides whether this machine can hold
	// the model and its KV cache alongside everything else.
	running, err := client.PS(ctx)
	if err != nil {
		t.Fatalf("PS: %v", err)
	}

	for _, m := range running {
		if strings.TrimSuffix(m.Name, ":latest") == strings.TrimSuffix(model, ":latest") {
			t.Logf("resident: %.2f GB total, %.2f GB on accelerator, context %d",
				float64(m.SizeBytes)/(1<<30), float64(m.SizeVRAMBytes)/(1<<30), m.ContextLength)
		}
	}
}

// TestSmokePlannerStructuredOutput is the important one: it measures whether
// schema-constrained planning output is actually valid, and what it costs.
//
// Thinking is left at the runtime default because it cannot be combined with a
// schema. The point of measuring is to see how much of the budget reasoning
// consumes before any answer appears.
func TestSmokePlannerStructuredOutput(t *testing.T) {
	client := smokeClient(t)

	model := smokeModel(t)

	const attempts = 3

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	valid := 0

	var totalWall, totalThinkingTokens int

	for i := range attempts {
		start := time.Now()

		res, err := client.Generate(ctx, ollama.GenerateRequest{
			Model: model,
			Prompt: "Plan this request as a task. Request: buy semi-skimmed milk before Friday, " +
				"it is urgent, and note that the shop closes at 6pm.",
			Schema:     plannerSchema,
			NumPredict: 512,
		})
		if err != nil {
			t.Fatalf("attempt %d: Generate: %v", i+1, err)
		}

		wall := time.Since(start)
		totalWall += int(wall)
		totalThinkingTokens += res.ThinkingTokens()

		var plan struct {
			Title    string   `json:"title"`
			Priority string   `json:"priority"`
			DueAt    string   `json:"due_at"`
			Steps    []string `json:"steps"`
		}

		err = json.Unmarshal([]byte(res.Response), &plan)

		switch {
		case err != nil:
			t.Logf("attempt %d: INVALID JSON after %6.2fs (%d tokens): %.120q", i+1, wall.Seconds(), res.EvalCount, res.Response)
		case plan.Title == "" || len(plan.Steps) == 0:
			t.Logf("attempt %d: parsed but incomplete after %6.2fs: %+v", i+1, wall.Seconds(), plan)
		default:
			valid++

			t.Logf("attempt %d: valid after %6.2fs (%d tokens, ~%d reasoning): title=%q priority=%q steps=%d",
				i+1, wall.Seconds(), res.EvalCount, res.ThinkingTokens(), plan.Title, plan.Priority, len(plan.Steps))
		}
	}

	rate := float64(valid) / float64(attempts)

	t.Logf("summary: %d/%d schema-valid (%.0f%%), avg %.2fs per plan, ~%d reasoning tokens per plan",
		valid, attempts, rate*100, float64(totalWall)/float64(attempts)/1e9, totalThinkingTokens/attempts)

	// This is the number that decides the planner design, so it is reported at
	// error level when poor: a low rate is not a failure of this harness, but it
	// should not scroll past unnoticed.
	if rate < 0.5 {
		t.Errorf("schema validity %.0f%% is below 50%%; prompt-plus-validation-and-retry is likely the better planner design", rate*100)
	}
}

// TestSmokeEmbedding checks the vector contract the database depends on.
func TestSmokeEmbedding(t *testing.T) {
	client := smokeClient(t)

	model := os.Getenv("BUDDI_SMOKE_EMBED_MODEL")
	if model == "" {
		model = "nomic-embed-text"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const wantDimensions = 768

	start := time.Now()

	vector, err := client.Embed(ctx, model, "Buy semi-skimmed milk before Friday.", wantDimensions)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	t.Logf("%s returned %d dimensions in %.2fs (requested %d)",
		model, len(vector), time.Since(start).Seconds(), wantDimensions)

	if len(vector) != wantDimensions {
		t.Fatalf("embedding width = %d, want %d to match the vector(768) column", len(vector), wantDimensions)
	}

	// A truncated vector loses the tail of the model output, so check it still
	// behaves like a unit-ish vector rather than assuming.
	var magnitude float64

	for _, v := range vector {
		magnitude += float64(v) * float64(v)
	}

	t.Logf("magnitude before normalisation: %.4f", magnitude)

	if magnitude == 0 {
		t.Error("embedding is all zeros, which would make every vector identical in the index")
	}
}

// TestSmokeResidentModels documents the keep-alive behaviour: after a
// generation the model should still be resident, which is what keeps the next
// request from paying the load cost again.
func TestSmokeResidentModels(t *testing.T) {
	client := smokeClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	running, err := client.PS(ctx)
	if err != nil {
		t.Fatalf("PS: %v", err)
	}

	if len(running) == 0 {
		t.Log("no models resident: keep_alive expired, so the next call reloads")

		return
	}

	for _, m := range running {
		t.Logf("resident: %-20s %.2f GB on device, context %d",
			m.Name, float64(m.SizeBytes)/(1<<30), m.ContextLength)
	}
}

// TestSmokePlannerFallbackOrder checks the deterministic path that the retry
// strategy depends on: prompt-only schema adherence must at least produce
// parseable JSON often enough to be worth validating.
func TestSmokePlannerFallbackOrder(t *testing.T) {
	client := smokeClient(t)

	model := smokeModel(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	prompt := fmt.Sprintf(`Reply with a single JSON object and nothing else.
Required keys: title (string), priority (one of low, medium, high), steps (array of strings).
Schema: %s

Request: buy semi-skimmed milk before Friday, it is urgent, and the shop closes at 6pm.`, plannerSchema)

	const attempts = 3

	parsed := 0

	for i := range attempts {
		start := time.Now()

		res, err := client.Generate(ctx, ollama.GenerateRequest{
			Model:      model,
			Prompt:     prompt,
			NumPredict: 512,
		})
		if err != nil {
			t.Fatalf("attempt %d: Generate: %v", i+1, err)
		}

		var plan map[string]any
		if json.Unmarshal([]byte(res.Response), &plan) == nil && plan["title"] != nil {
			parsed++

			t.Logf("attempt %d: parsed in %.2fs without schema support", i+1, time.Since(start).Seconds())

			continue
		}

		t.Logf("attempt %d: unparseable in %.2fs: %.120q", i+1, time.Since(start).Seconds(), res.Response)
	}

	t.Logf("summary: %d/%d parsed without schema support", parsed, attempts)
}

package planner_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
)

// streamingGenerator is a generator that can also stream, which is what the real
// Ollama adapter does.
type streamingGenerator struct {
	fakeGenerator

	deltas []planner.Delta

	// onDelta lets a test fail the stream part way through.
	onDelta func(planner.Delta) error
}

func (s *streamingGenerator) GenerateStream(
	_ context.Context,
	req planner.Request,
	onDelta func(planner.Delta) error,
) (planner.Response, error) {
	s.calls++
	s.prompts = append(s.prompts, req.Prompt)
	s.schemas = append(s.schemas, string(req.Schema))

	for _, delta := range s.deltas {
		if err := onDelta(delta); err != nil {
			return planner.Response{}, err
		}
	}

	i := s.calls - 1

	if i < len(s.errs) && s.errs[i] != nil {
		return planner.Response{}, s.errs[i]
	}

	if i < len(s.responses) {
		return s.responses[i], nil
	}

	return planner.Response{}, nil
}

func newStreamingService(t *testing.T, gen *streamingGenerator, opts planner.Options) *planner.Service {
	t.Helper()

	if opts.MaxRetries == 0 {
		opts.MaxRetries = 1
	}

	service, err := planner.NewService(gen, opts)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	return service
}

// A streamed plan must still be validated, not trusted: a schema constrains shape,
// not meaning, so the same parse has to run either way.
func TestStreamedPlanIsStillValidated(t *testing.T) {
	gen := &streamingGenerator{
		fakeGenerator: fakeGenerator{
			responses: []planner.Response{
				{Text: `{"title":"","priority":"high","steps":["one"]}`},
				{Text: `{"title":"Buy milk","priority":"high","steps":["one"]}`},
			},
		},
		deltas: []planner.Delta{{Text: `{"title":"`}, {Text: `Buy milk"`}},
	}

	var seen []string

	service := newStreamingService(t, gen, planner.Options{}).
		WithEmitter(func(d planner.Delta) error {
			seen = append(seen, d.Text)
			return nil
		})

	outcome, err := service.Plan(context.Background(), testUserID, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// The empty title is fatal, so the planner must have retried rather than
	// accepting the first streamed answer.
	if outcome.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2: an invalid plan must be retried", outcome.Attempts)
	}

	if outcome.Plan == nil || outcome.Plan.Title != "Buy milk" {
		t.Fatalf("Plan = %+v, want the retried answer", outcome.Plan)
	}

	if outcome.UsedFallback {
		t.Error("UsedFallback = true, want a real plan")
	}

	if len(seen) == 0 {
		t.Error("no deltas were emitted")
	}
}

// Reasoning is carried through so a caller can show it, which is the whole reason
// the runtime was asked for it.
func TestStreamedReasoningReachesTheOutcome(t *testing.T) {
	gen := &streamingGenerator{
		fakeGenerator: fakeGenerator{
			responses: []planner.Response{{
				Text:      `{"title":"Buy milk","priority":"high","steps":["one"]}`,
				Reasoning: "the request names a deadline",
			}},
		},
		deltas: []planner.Delta{{Reasoning: "the request "}, {Reasoning: "names a deadline"}},
	}

	var reasoning []string

	service := newStreamingService(t, gen, planner.Options{}).
		WithEmitter(func(d planner.Delta) error {
			reasoning = append(reasoning, d.Reasoning)
			return nil
		})

	outcome, err := service.Plan(context.Background(), testUserID, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if outcome.Reasoning != "the request names a deadline" {
		t.Errorf("Reasoning = %q, want the model's own text", outcome.Reasoning)
	}

	if len(reasoning) != 2 {
		t.Errorf("emitted %d reasoning deltas, want 2", len(reasoning))
	}
}

// A client that stops reading must stop the model. Otherwise a closed browser tab
// leaves the runtime generating an answer for nobody.
func TestEmitterErrorStopsPlanning(t *testing.T) {
	gen := &streamingGenerator{
		fakeGenerator: fakeGenerator{
			responses: []planner.Response{
				{Text: `{"title":"Buy milk","priority":"high","steps":["one"]}`},
				{Text: `{"title":"Buy milk again","priority":"high","steps":["one"]}`},
			},
		},
		deltas: []planner.Delta{{Text: `{"title":"Buy`}, {Text: ` milk"`}},
	}

	calls := 0

	service := newStreamingService(t, gen, planner.Options{}).
		WithEmitter(func(planner.Delta) error {
			calls++
			return errors.New("client disconnected")
		})

	if _, err := service.Plan(context.Background(), testUserID, "buy milk"); err == nil {
		t.Error("Plan returned no error after the emitter failed")
	}

	if calls != 1 {
		t.Errorf("emitter called %d times, want 1: the stream must stop at the first failure", calls)
	}
}

// A generator that cannot stream must still work. This is the path every existing
// caller and every fake in the suite takes, so a regression here would be silent
// until the model layer was swapped back out.
func TestPlannerFallsBackToBufferedGeneration(t *testing.T) {
	gen := &streamingGenerator{
		fakeGenerator: fakeGenerator{
			responses: []planner.Response{
				{Text: `{"title":"Buy milk","priority":"high","steps":["one"]}`},
			},
		},
	}

	var emitted int

	// The service is built with the non-streaming type on purpose.
	service, err := planner.NewService(gen.buffered(), planner.Options{MaxRetries: 1})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	service.WithEmitter(func(planner.Delta) error {
		emitted++
		return nil
	})

	outcome, err := service.Plan(context.Background(), testUserID, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if outcome.Plan == nil || outcome.Plan.Title != "Buy milk" {
		t.Fatalf("Plan = %+v", outcome.Plan)
	}

	if emitted != 0 {
		t.Errorf("emitted %d deltas from a buffered generator, want 0", emitted)
	}

	if gen.calls != 1 {
		t.Errorf("Generate called %d times, want 1", gen.calls)
	}
}

// buffered hides the streaming method so the service sees a plain Generator.
func (s *streamingGenerator) buffered() *fakeGenerator {
	return &s.fakeGenerator
}

var _ = uuid.Nil

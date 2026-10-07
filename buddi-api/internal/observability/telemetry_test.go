package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

// The rewrite exists so a configured duration is readable in the log rather than
// an integer nanosecond count. These check the rendered JSON, not the function,
// because the function's return value alone would pass even if the handler ignored
// it.
func TestJSONHandlerRendersDurationsAsText(t *testing.T) {
	record := decode(t,
		"indexing worker started",
		"batch_size", 20,
		"interval", 5*time.Second,
		"lease", 2*time.Minute,
	)

	if got := field(t, record, "lease"); got != "2m0s" {
		t.Errorf("lease = %v, want 2m0s", got)
	}

	if got := field(t, record, "interval"); got != "5s" {
		t.Errorf("interval = %v, want 5s", got)
	}
}

// A non-duration must pass through unchanged. A rewrite that also caught other
// types would turn counts into strings and break anything reading the logs.
func TestJSONHandlerLeavesOtherValuesAlone(t *testing.T) {
	record := decode(t,
		"indexing worker started",
		"batch_size", 20,
		"ratio", 0.25,
		"enabled", true,
	)

	if got := field(t, record, "batch_size"); got != float64(20) {
		t.Errorf("batch_size = %#v, want 20", got)
	}

	if got := field(t, record, "ratio"); got != 0.25 {
		t.Errorf("ratio = %#v, want 0.25", got)
	}

	if got := field(t, record, "enabled"); got != true {
		t.Errorf("enabled = %#v, want true", got)
	}
}

// Zero is the value a nil timeout arrives as, and it is also what a missed field
// would look like if the rewrite dropped it instead of formatting it.
func TestJSONHandlerRendersZeroDuration(t *testing.T) {
	record := decode(t, "no timeout configured", "timeout", time.Duration(0))

	if got := field(t, record, "timeout"); got != "0s" {
		t.Errorf("timeout = %v, want 0s", got)
	}
}

// A duration large enough to need a larger unit is reported in that unit rather
// than as milliseconds, so a two minute lease is not read as 120000ms.
func TestJSONHandlerKeepsPrecision(t *testing.T) {
	record := decode(t, "lease", "lease", 90*time.Second)

	if got := field(t, record, "lease"); got != "1m30s" {
		t.Errorf("lease = %v, want 1m30s", got)
	}
}

// decode renders one record through the same handler Setup installs and returns it
// parsed.
func decode(t *testing.T, message string, args ...any) map[string]any {
	t.Helper()

	var buf bytes.Buffer

	logger := slog.New(TraceHandler{Handler: newJSONHandler(&buf, slog.LevelDebug)})
	logger.Info(message, args...)

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}

	return record
}

func field(t *testing.T, record map[string]any, key string) any {
	t.Helper()

	value, ok := record[key]
	if !ok {
		t.Fatalf("field %q missing from %v", key, record)
	}

	return value
}

// The signal path must be appended to a base endpoint. Without it the exporter
// POSTs to the collector root and every batch is answered with 404, which is a
// silent failure: traces simply never arrive.
func TestTracesEndpointAppendsTheSignalPath(t *testing.T) {
	cases := map[string]string{
		"http://localhost:4318":           "http://localhost:4318/v1/traces",
		"http://localhost:4318/":          "http://localhost:4318/v1/traces",
		"http://localhost:4318/v1/traces": "http://localhost:4318/v1/traces",
		"https://collector.example.com":   "https://collector.example.com/v1/traces",
	}

	for base, want := range cases {
		if got := tracesEndpoint(base); got != want {
			t.Errorf("tracesEndpoint(%q) = %q, want %q", base, got, want)
		}
	}
}

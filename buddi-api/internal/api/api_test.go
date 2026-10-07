package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

type stubPinger struct {
	err error
}

func (s stubPinger) Ping(context.Context) error { return s.err }

func newTestAPI(t *testing.T, opts Options) *API {
	t.Helper()

	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return New(opts)
}

func TestHealthReturnsOK(t *testing.T) {
	handler := newTestAPI(t, Options{}).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestReadyReflectsDatabaseState(t *testing.T) {
	tests := map[string]struct {
		pinger stubPinger
		want   int
	}{
		"reachable": {pinger: stubPinger{}, want: http.StatusOK},
		"failing":   {pinger: stubPinger{err: errors.New("down")}, want: http.StatusServiceUnavailable},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			handler := newTestAPI(t, Options{Database: tt.pinger}).Handler()

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestUnknownRouteReturnsJSONNotFound(t *testing.T) {
	handler := newTestAPI(t, Options{}).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if body.Error.Code != "not_found" {
		t.Fatalf("code = %q, want %q", body.Error.Code, "not_found")
	}

	if body.Error.RequestID == "" {
		t.Fatal("expected request id in error body")
	}
}

func TestRecovererTurnsPanicInto500(t *testing.T) {
	handler := Chain(Recoverer(slog.New(slog.NewTextHandler(io.Discard, nil))), RequestID())(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestRequestIDPropagates(t *testing.T) {
	var seen string

	handler := Chain(RequestID())(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "abc-123")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if seen != "abc-123" {
		t.Fatalf("request id = %q, want %q", seen, "abc-123")
	}

	if got := rec.Header().Get("X-Request-ID"); got != "abc-123" {
		t.Fatalf("response header = %q, want %q", got, "abc-123")
	}
}

func TestCORSAllowedOriginAndPreflight(t *testing.T) {
	handler := Chain(CORS([]string{"https://app.example.com"}))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		}),
	)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/users", nil)
	req.Header.Set("Origin", "https://app.example.com")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want %d", rec.Code, http.StatusNoContent)
	}

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("allow origin = %q, want %q", got, "https://app.example.com")
	}
}

func TestCORSRejectsUnknownOrigin(t *testing.T) {
	handler := Chain(CORS([]string{"https://app.example.com"}))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.Header.Set("Origin", "https://evil.example.com")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("allow origin = %q, want empty", got)
	}
}

func TestWriteErrorMapsPersistenceSentinels(t *testing.T) {
	tests := map[string]struct {
		err    error
		status int
		code   string
	}{
		"not found":        {err: persistence.ErrNotFound, status: http.StatusNotFound, code: "not_found"},
		"conflict":         {err: persistence.ErrConflict, status: http.StatusConflict, code: "conflict"},
		"wrapped notfound": {err: wrap(persistence.ErrNotFound), status: http.StatusNotFound, code: "not_found"},
		"unknown":          {err: errors.New("boom"), status: http.StatusInternalServerError, code: "internal_error"},

		// The model runtime is a dependency. Exhausting its budget is a 504, not
		// a 500, because a client retrying a 504 is behaving correctly whereas a
		// client retrying a 500 is usually just adding load.
		"deadline": {err: context.DeadlineExceeded, status: http.StatusGatewayTimeout, code: "gateway_timeout"},
		"wrapped deadline": {
			err:    fmt.Errorf("agent: plan: %w", context.DeadlineExceeded),
			status: http.StatusGatewayTimeout, code: "gateway_timeout",
		},
		"cancelled": {err: context.Canceled, status: 499, code: "client_closed_request"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteError(rec, httptest.NewRequest(http.MethodGet, "/", nil), tt.err)

			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}

			var body errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}

			if body.Error.Code != tt.code {
				t.Fatalf("code = %q, want %q", body.Error.Code, tt.code)
			}
		})
	}
}

func TestWriteServiceErrorMapsADeadlineToGatewayTimeout(t *testing.T) {
	// The service handlers use writeServiceError, not WriteError. Testing only the
	// latter left a planning timeout answering 500 in production while its unit
	// test passed.
	tests := map[string]struct {
		err    error
		status int
		code   string
	}{
		"deadline":         {err: context.DeadlineExceeded, status: http.StatusGatewayTimeout, code: "gateway_timeout"},
		"wrapped deadline": {err: fmt.Errorf("agent: plan: %w", context.DeadlineExceeded), status: http.StatusGatewayTimeout, code: "gateway_timeout"},
		"unwrapped":        {err: domain.ErrInvalid, status: http.StatusUnprocessableEntity, code: "validation_failed"},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			server := &API{log: log}
			rec := httptest.NewRecorder()

			server.writeServiceError(rec, httptest.NewRequest(http.MethodPost, "/", nil), tt.err)

			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}

			var body errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}

			if body.Error.Code != tt.code {
				t.Errorf("code = %q, want %q", body.Error.Code, tt.code)
			}
		})
	}
}

func TestWriteErrorHidesInternalDetail(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest(http.MethodGet, "/", nil), errors.New("connection string leaked"))

	if strings.Contains(rec.Body.String(), "connection string leaked") {
		t.Fatalf("internal detail leaked: %s", rec.Body.String())
	}
}

func TestDecodeJSON(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}

	t.Run("valid", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"ada"}`))
		req.Header.Set("Content-Type", "application/json")

		var got payload
		if err := decodeJSON(req, &got); err != nil {
			t.Fatalf("decodeJSON: %v", err)
		}

		if got.Name != "ada" {
			t.Fatalf("name = %q, want %q", got.Name, "ada")
		}
	})

	invalid := map[string]struct {
		body        string
		contentType string
	}{
		"malformed":       {body: `{"name":`, contentType: "application/json"},
		"empty body":      {body: ``, contentType: "application/json"},
		"unknown field":   {body: `{"name":"ada","role":"admin"}`, contentType: "application/json"},
		"wrong type":      {body: `{"name":42}`, contentType: "application/json"},
		"wrong media":     {body: `{"name":"ada"}`, contentType: "text/plain"},
		"trailing object": {body: `{"name":"ada"}{"name":"bob"}`, contentType: "application/json"},
	}

	for name, tt := range invalid {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)

			var got payload
			if err := decodeJSON(req, &got); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestPagination(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		opts, err := Pagination(httptest.NewRequest(http.MethodGet, "/", nil))
		if err != nil {
			t.Fatalf("Pagination: %v", err)
		}

		if opts.Limit != persistence.DefaultLimit || opts.Offset != 0 {
			t.Fatalf("opts = %+v, want limit %d and no offset", opts, persistence.DefaultLimit)
		}
	})

	t.Run("explicit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?limit=10&offset=20&order=created_at+desc", nil)

		opts, err := Pagination(req)
		if err != nil {
			t.Fatalf("Pagination: %v", err)
		}

		if opts.Limit != 10 || opts.Offset != 20 || opts.Order != "created_at desc" {
			t.Fatalf("opts = %+v", opts)
		}
	})

	for _, query := range []string{"?limit=0", "?limit=500", "?limit=abc", "?offset=-1"} {
		t.Run("rejects "+query, func(t *testing.T) {
			if _, err := Pagination(httptest.NewRequest(http.MethodGet, "/", nil)); err != nil {
				t.Fatalf("baseline Pagination returned error: %v", err)
			}

			req := httptest.NewRequest(http.MethodGet, "/"+query, nil)
			if _, err := Pagination(req); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func wrap(err error) error {
	return &wrapped{err: err}
}

type wrapped struct{ err error }

func (w *wrapped) Error() string { return "wrapped: " + w.err.Error() }

func (w *wrapped) Unwrap() error { return w.err }

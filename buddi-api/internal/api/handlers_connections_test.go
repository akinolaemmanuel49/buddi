package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/oauth"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// stubConnections reports a status error, standing in for the unconnected and revoked
// sentinels the oauth service returns.
type stubConnections struct {
	status oauth.Connection
	err    error
}

func (s stubConnections) Status(
	context.Context, uuid.UUID, domain.Provider,
) (oauth.Connection, error) {
	return s.status, s.err
}

func (s stubConnections) Connect(
	context.Context, uuid.UUID, domain.Provider, oauth.Grant,
) error {
	return nil
}

func (s stubConnections) Disconnect(context.Context, uuid.UUID, domain.Provider) error {
	return nil
}

// Asking whether a provider is connected is the normal case for an unused connector,
// so it must not be reported as a server error: a 500 makes it look broken rather than
// merely not connected yet.
func TestConnectionStatusReportsNotConnectedAsAnAnswer(t *testing.T) {
	for name, err := range map[string]error{
		"never connected": oauth.ErrNotConnected,
		"revoked":         oauth.ErrRevoked,
	} {
		t.Run(name, func(t *testing.T) {
			handler := newTestAPI(t, Options{
				Auth:        stubAuthenticator{user: chatTestUser},
				Connections: stubConnections{err: err},
			}).Handler()

			req := httptest.NewRequest(
				http.MethodGet, "/api/v1/connections/google_calendar", nil)
			req.Header.Set("Authorization", "Bearer test")

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}

			var body connectionResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}

			if body.Connected {
				t.Error("connected = true, want false")
			}

			if body.Provider != "google_calendar" {
				t.Errorf("Provider = %q, want it echoed so the client knows which", body.Provider)
			}
		})
	}
}

func TestConnectionStatusSurfacesARealFailure(t *testing.T) {
	handler := newTestAPI(t, Options{
		Auth:        stubAuthenticator{user: chatTestUser},
		Connections: stubConnections{err: errors.New("database is on fire")},
	}).Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/connections/google_calendar", nil)
	req.Header.Set("Authorization", "Bearer test")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for an unrecognised failure", rec.Code)
	}
}

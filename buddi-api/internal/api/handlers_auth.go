package api

import (
	"net/http"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/auth"
)

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// userResponse is the public shape of a user. The password hash is never part
// of it, and the domain type hides it from JSON as well; this struct exists so
// adding a field to the entity cannot accidentally publish it.
type userResponse struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

func toUserResponse(u *auth.Session) userResponse {
	return userResponse{
		ID:          u.User.ID.String(),
		Email:       u.User.Email,
		DisplayName: u.User.DisplayName,
	}
}

func (a *API) handleRegister(w http.ResponseWriter, r *http.Request) {
	var body registerRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)

		return
	}

	session, err := a.auth.Register(r.Context(), auth.RegisterInput{
		Email:       body.Email,
		Password:    body.Password,
		DisplayName: body.DisplayName,
		Meta:        tokenMetaFromRequest(r),
	})
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusCreated, a.sessionResponse(session))
}

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body loginRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)

		return
	}

	session, err := a.auth.Login(r.Context(), body.Email, body.Password, tokenMetaFromRequest(r))
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusOK, a.sessionResponse(session))
}

func (a *API) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var body refreshRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)

		return
	}

	session, err := a.auth.Refresh(r.Context(), body.RefreshToken, tokenMetaFromRequest(r))
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusOK, a.sessionResponse(session))
}

// handleLogout revokes a refresh token. It answers 204 whether or not the token
// was known, so a client can always clear its local credentials.
func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	var body refreshRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)

		return
	}

	if err := a.auth.Logout(r.Context(), body.RefreshToken); err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleMe returns the authenticated user, which doubles as a way for a client
// to check whether its access token is still good.
func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok || user == nil {
		WriteError(w, r, Internal("authenticated route reached without a user on the context"))

		return
	}

	WriteJSON(w, r, http.StatusOK, userResponse{
		ID:          user.ID.String(),
		Email:       user.Email,
		DisplayName: user.DisplayName,
	})
}

type sessionResponse struct {
	AccessToken  string       `json:"access_token"`
	RefreshToken string       `json:"refresh_token"`
	TokenType    string       `json:"token_type"`
	ExpiresAt    string       `json:"expires_at"`
	User         userResponse `json:"user"`
}

func (a *API) sessionResponse(session *auth.Session) sessionResponse {
	return sessionResponse{
		AccessToken:  session.AccessToken,
		RefreshToken: session.RefreshToken,
		TokenType:    session.TokenType,
		ExpiresAt:    session.ExpiresAt.UTC().Format(timeLayout),
		User:         toUserResponse(session),
	}
}

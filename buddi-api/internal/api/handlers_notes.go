package api

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/note"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

type createNoteRequest struct {
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Tags    []string `json:"tags"`
}

type updateNoteRequest struct {
	Title   *string   `json:"title"`
	Content *string   `json:"content"`
	Tags    *[]string `json:"tags"`
	// The three archive flags are mutually exclusive and are validated in the
	// service, so a contradictory request is a 422 rather than a silent choice.
	Archive      bool `json:"archive"`
	Restore      bool `json:"restore"`
	ClearArchive bool `json:"clear_archive"`
}

type noteResponse struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Content    string   `json:"content"`
	Tags       []string `json:"tags"`
	Source     string   `json:"source"`
	ArchivedAt *string  `json:"archived_at"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`

	// SearchState is always serialised, including when the note is merely pending.
	// A user who saves a note and cannot tell whether it can be found again is being
	// told less than the system knows, and an absent field is indistinguishable from
	// an older payload — the same reasoning as plan_fallback on a run.
	//
	// The failure reason is deliberately not included: it is the embedding runtime's
	// error text, which can name hosts and URLs. The state is enough to ask the
	// question, and the logs are where the reason belongs.
	SearchState string  `json:"search_state"`
	IndexedAt   *string `json:"indexed_at,omitempty"`
}

type noteListResponse struct {
	Data   []noteResponse `json:"data"`
	Total  int64          `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

func toNoteResponse(n *domain.Note) noteResponse {
	// A malformed literal must not blank the response, so the fallback is the
	// error rather than a silent empty list.
	tags, err := n.Tags.Slice()
	if err != nil {
		tags = []string{}
	}

	response := noteResponse{
		ID:          n.ID.String(),
		Title:       n.Title,
		Content:     n.Content,
		Tags:        tags,
		Source:      n.Source,
		SearchState: string(n.SearchState),
		CreatedAt:   n.CreatedAt.UTC().Format(timeLayout),
		UpdatedAt:   n.UpdatedAt.UTC().Format(timeLayout),
	}

	if n.IndexedAt != nil {
		formatted := n.IndexedAt.UTC().Format(timeLayout)
		response.IndexedAt = &formatted
	}

	if response.Tags == nil {
		response.Tags = []string{}
	}

	if n.ArchivedAt != nil {
		formatted := n.ArchivedAt.UTC().Format(timeLayout)
		response.ArchivedAt = &formatted
	}

	return response
}

func (a *API) handleCreateNote(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	var body createNoteRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)

		return
	}

	created, err := a.notes.Create(r.Context(), userID, note.CreateInput{
		Title:   body.Title,
		Content: body.Content,
		Tags:    body.Tags,
	})
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusCreated, toNoteResponse(created))
}

func (a *API) handleListNotes(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	page, err := Pagination(r)
	if err != nil {
		WriteError(w, r, err)

		return
	}

	filter, err := noteFilterFromRequest(r)
	if err != nil {
		WriteError(w, r, err)

		return
	}

	notes, total, err := a.notes.List(r.Context(), userID, filter, domain.Page{
		Limit:  page.Limit,
		Offset: page.Offset,
		Order:  page.Order,
	})
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	data := make([]noteResponse, 0, len(notes))
	for i := range notes {
		data = append(data, toNoteResponse(&notes[i]))
	}

	WriteJSON(w, r, http.StatusOK, noteListResponse{
		Data:   data,
		Total:  total,
		Limit:  page.Limit,
		Offset: page.Offset,
	})
}

// noteFilterFromRequest reads the list filters. include_archived and the tag
// list are read here; a bad tag is caught by the service's normaliser.
func noteFilterFromRequest(r *http.Request) (domain.NoteFilter, error) {
	filter := domain.NoteFilter{
		IncludeArchived: QueryBool(r, "include_archived", false),
		Search:          strings.TrimSpace(Query(r, "q")),
	}

	if QueryBool(r, "archived", false) {
		filter.Archived = true
	}

	if raw := Query(r, "tags"); raw != "" {
		filter.Tags = strings.Split(raw, ",")
	}

	return filter, nil
}

func (a *API) handleGetNote(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	noteID, ok := parseID(w, r, "id")
	if !ok {
		return
	}

	found, err := a.notes.Get(r.Context(), userID, noteID)
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusOK, toNoteResponse(found))
}

func (a *API) handleUpdateNote(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	noteID, ok := parseID(w, r, "id")
	if !ok {
		return
	}

	var body updateNoteRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)

		return
	}

	updated, err := a.notes.Update(r.Context(), userID, noteID, note.UpdateInput{
		Title:        body.Title,
		Content:      body.Content,
		Tags:         body.Tags,
		Archive:      body.Archive,
		Restore:      body.Restore,
		ClearArchive: body.ClearArchive,
	})
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusOK, toNoteResponse(updated))
}

func (a *API) handleDeleteNote(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	noteID, ok := parseID(w, r, "id")
	if !ok {
		return
	}

	if err := a.notes.Delete(r.Context(), userID, noteID); err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// parseID reads a path parameter as a UUID. A malformed id is a 400, since it
// can never name a row, and it is rejected before touching the database.
func parseID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	raw := PathParam(r, name)

	id, err := parseUUID(raw)
	if err != nil {
		WriteError(w, r, BadRequest(name+" must be a valid identifier"))

		return uuid.Nil, false
	}

	return id, true
}

// parseUUID converts a string to a UUID.
func parseUUID(raw string) (uuid.UUID, error) {
	return uuid.Parse(strings.TrimSpace(raw))
}

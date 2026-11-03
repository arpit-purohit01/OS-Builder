package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"foundry.dev/foundry/internal/osspec"
	"foundry.dev/foundry/internal/store"
)

// Problem is an RFC 9457 problem document.
type Problem struct {
	Type      string              `json:"type"`
	Title     string              `json:"title"`
	Status    int                 `json:"status"`
	Detail    string              `json:"detail,omitempty"`
	Instance  string              `json:"instance,omitempty"`
	RequestID string              `json:"requestId,omitempty"`
	Layer     string              `json:"layer,omitempty"`
	Errors    []osspec.FieldError `json:"errors,omitempty"`
	Warnings  []osspec.FieldError `json:"warnings,omitempty"`
}

const problemBase = "https://docs.foundry.dev/errors/"

func problem(w http.ResponseWriter, r *http.Request, status int, typ, title, detail string) {
	writeProblem(w, r, Problem{Type: problemBase + typ, Title: title, Status: status, Detail: detail})
}

func writeProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	if r != nil {
		p.Instance = r.URL.Path
		p.RequestID = requestID(r.Context())
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	json.NewEncoder(w).Encode(p)
}

// fail maps common errors to problem responses.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ve *osspec.ValidationError
	switch {
	case errors.As(err, &ve):
		status := 422
		if ve.Layer == "policy" {
			status = 403
		}
		writeProblem(w, r, Problem{Type: problemBase + "spec-invalid", Title: "Spec is invalid", Status: status,
			Detail: "The OS specification failed " + ve.Layer + " validation.", Layer: ve.Layer, Errors: ve.Errors})
	case errors.Is(err, store.ErrNotFound):
		problem(w, r, 404, "not-found", "Not found", "")
	case errors.Is(err, store.ErrConflict):
		problem(w, r, 409, "conflict", "Conflict", err.Error())
	case errors.Is(err, store.ErrIllegalTransition):
		problem(w, r, 409, "illegal-state", "Not allowed in the current state", err.Error())
	default:
		s.log.Error("request failed", "err", err, "path", r.URL.Path, "rid", requestID(r.Context()))
		problem(w, r, 500, "internal", "Internal error", "")
	}
}

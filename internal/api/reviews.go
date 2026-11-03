package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"foundry.dev/foundry/internal/jobs"
	"foundry.dev/foundry/internal/queue"
	"foundry.dev/foundry/internal/store"
	"github.com/jackc/pgx/v5"
)

func init() {
	extraRouteFns = append(extraRouteFns, func(s *Server, mux *http.ServeMux) {
		s.route(mux, "GET /v1/orgs/{org}/reviews", s.org("reviewer", "reviews:write", s.reviewQueue))
		s.route(mux, "GET /v1/orgs/{org}/builds/{build}/security", s.org("viewer", "builds:read", s.buildSecurity))
		s.route(mux, "POST /v1/orgs/{org}/builds/{build}/reviews", s.org("reviewer", "reviews:write", s.createReview))
		s.route(mux, "POST /v1/orgs/{org}/builds/{build}/findings/{finding}/accept", s.org("reviewer", "reviews:write", s.acceptFinding))
	})
}

// ReviewersRequired implements the two-person rule (security-threat-model.md §6).
func ReviewersRequired(risk *int) int {
	if risk != nil && *risk > 60 {
		return 2
	}
	return 1
}

func (s *Server) reviewQueue(w http.ResponseWriter, r *http.Request) {
	bs, err := s.db.ReviewQueue(r.Context(), getPrincipal(r.Context()).OrgID, limitParam(r, 50, 200))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if bs == nil {
		bs = []store.Build{}
	}
	writeJSON(w, 200, map[string]any{"items": bs})
}

// buildSecurity returns everything a reviewer needs: scans, findings,
// egress attempts, reviews and the required number of approvals.
func (s *Server) buildSecurity(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	b, err := s.db.GetBuild(r.Context(), p.OrgID, r.PathValue("build"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	scans, _ := s.db.Scans(r.Context(), b.ID)
	findings, _ := s.db.Findings(r.Context(), b.ID)
	egress, _ := s.db.Egress(r.Context(), b.ID, false)
	reviews, _ := s.db.Reviews(r.Context(), b.ID)
	nz := func(v any) any { return v }
	writeJSON(w, 200, map[string]any{"build": b, "scans": nz(orEmpty(scans)), "findings": nz(orEmpty(findings)), "egress": nz(orEmpty(egress)),
		"reviews": nz(orEmpty(reviews)), "reviewersRequired": ReviewersRequired(b.RiskScore)})
}

func orEmpty[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

func (s *Server) createReview(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	var req struct {
		Decision string `json:"decision"`
		Comment  string `json:"comment"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	switch req.Decision {
	case "approve", "reject", "changes_requested":
	default:
		problem(w, r, 422, "invalid", "decision must be approve, reject or changes_requested", "")
		return
	}
	if req.Decision != "approve" && strings.TrimSpace(req.Comment) == "" {
		problem(w, r, 422, "invalid", "A comment is required when rejecting or requesting changes", "")
		return
	}
	if len(req.Comment) > 4000 {
		problem(w, r, 422, "invalid", "Comment too long", "")
		return
	}
	b, err := s.db.GetBuild(r.Context(), p.OrgID, r.PathValue("build"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if b.State != store.StateAwaitingReview {
		problem(w, r, 409, "not-reviewable", "Build is not awaiting review", "State: "+b.State)
		return
	}
	if b.CreatedBy == p.UserID {
		problem(w, r, 403, "separation-of-duties", "You cannot review your own build", "")
		return
	}
	need := ReviewersRequired(b.RiskScore)
	var outcome string
	err = s.db.Tx(r.Context(), func(tx pgx.Tx) error {
		// lock the build row so concurrent approvals count exactly once
		var state string
		if err := tx.QueryRow(r.Context(), `SELECT state FROM builds WHERE id=$1 FOR UPDATE`, b.ID).Scan(&state); err != nil {
			return err
		}
		if state != store.StateAwaitingReview {
			return store.ErrConflict
		}
		rev := &store.Review{BuildID: b.ID, Reviewer: p.UserID, Decision: req.Decision, Comment: req.Comment}
		if err := s.db.AddReview(r.Context(), tx, rev); err != nil {
			return err
		}
		actor := "user:" + p.UserID
		switch req.Decision {
		case "reject":
			outcome = store.StateRejected
			return store.Transition(r.Context(), tx, b.ID, state, store.StateRejected, store.TransitionOpts{Actor: actor, Detail: map[string]string{"comment": req.Comment}})
		case "changes_requested":
			outcome = store.StateChangesRequested
			return store.Transition(r.Context(), tx, b.ID, state, store.StateChangesRequested, store.TransitionOpts{Actor: actor, Detail: map[string]string{"comment": req.Comment}})
		}
		var approvals int
		if err := tx.QueryRow(r.Context(), `SELECT count(DISTINCT reviewer) FROM reviews WHERE build_id=$1 AND decision='approve'`, b.ID).Scan(&approvals); err != nil {
			return err
		}
		if approvals < need {
			outcome = "awaiting_review"
			return nil
		}
		tier := "reviewed"
		outcome = store.StateApproved
		if err := store.Transition(r.Context(), tx, b.ID, state, store.StateApproved, store.TransitionOpts{Actor: actor, Tier: &tier,
			Detail: map[string]any{"approvals": approvals}}); err != nil {
			return err
		}
		return queue.Enqueue(r.Context(), tx, queue.Job{Kind: jobs.Finalize, OrgID: b.OrgID, Payload: jobs.Payload(b.ID), UniqueKey: "finalize:" + b.ID})
	})
	if errors.Is(err, store.ErrConflict) {
		problem(w, r, 409, "conflict", "You already reviewed this build, or it was decided meanwhile", "")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r.Context(), p.OrgID, p.UserID, "build.review."+req.Decision, b.ID, map[string]string{"comment": req.Comment})
	s.Notify(b.ID)
	resp, _ := json.Marshal(map[string]any{"state": outcome, "reviewersRequired": need})
	w.Header().Set("Content-Type", "application/json")
	w.Write(resp)
}

func (s *Server) acceptFinding(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	var req struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		problem(w, r, 422, "invalid", "A reason is required", "")
		return
	}
	b, err := s.db.GetBuild(r.Context(), p.OrgID, r.PathValue("build"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.db.AcceptFinding(r.Context(), b.ID, r.PathValue("finding"), p.UserID, req.Reason); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r.Context(), p.OrgID, p.UserID, "finding.accept", r.PathValue("finding"), map[string]string{"build": b.ID, "reason": req.Reason})
	w.WriteHeader(204)
}

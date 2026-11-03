package api

import "context"

// audit records an action; failures are logged, never fatal to the request.
func (s *Server) audit(ctx context.Context, orgID, actor, action, target string, detail any) {
	if err := s.db.Audit(context.WithoutCancel(ctx), orgID, actor, action, target, detail); err != nil {
		s.log.Error("audit", "err", err, "action", action)
	}
}

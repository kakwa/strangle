package server

import (
	"context"
	"errors"
	"net/http"
)

// restricted reports whether collection memberships are in effect. While no
// membership exists, every authenticated user keeps access to all
// collections: setting up the first UI account must not lock the UI. A store
// error fails closed.
func (s *Server) restricted(ctx context.Context) bool {
	if !s.authOn || s.access == nil {
		return false
	}
	any, err := s.access.HasAny(ctx)
	if err != nil {
		return true
	}
	return any
}

// accessibleTargets returns the set of collection names the caller may see.
// A nil set means unrestricted. An empty set means no collection is visible.
func (s *Server) accessibleTargets(ctx context.Context, r *http.Request) map[string]bool {
	if !s.restricted(ctx) {
		return nil
	}
	user, ok := userFromRequest(r)
	if !ok {
		return map[string]bool{}
	}
	memberships, err := s.access.List(ctx, user)
	if err != nil {
		return map[string]bool{}
	}
	out := make(map[string]bool, len(memberships))
	for _, m := range memberships {
		out[m.Collection] = true
	}
	return out
}

// targetVisible reports whether a record of collection target is visible.
func targetVisible(allowed map[string]bool, target string) bool {
	return allowed == nil || allowed[target]
}

// mayUse reports whether the caller may access one org/collection pair.
// Collection-scoped webhook syncs remain usable with the static sync token,
// which is a machine identity rather than a user.
func (s *Server) mayUse(ctx context.Context, r *http.Request, org, collection string) bool {
	if !s.restricted(ctx) {
		return true
	}
	if user, ok := userFromRequest(r); ok {
		ok, err := s.access.Allows(ctx, user, org, collection)
		return err == nil && ok
	}
	return s.syncOK(r)
}

// withCollectionAccess wraps a collection-scoped handler with the membership
// check. Handlers on the flat prefix have no org/collection path parameters
// and are filtered per record instead.
func (s *Server) withCollectionAccess(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		collection := r.PathValue("collection")
		if collection != "" && !s.mayUse(r.Context(), r, r.PathValue("org"), collection) {
			writeError(w, http.StatusForbidden, errors.New("access to this collection is denied"))
			return
		}
		next(w, r)
	}
}

// collectionTarget returns the org a configured collection belongs to, so
// flat routes carrying only a target name can be checked too.
func (s *Server) collectionTarget(target string) (string, bool) {
	if s.applier == nil {
		return "", false
	}
	t, ok := s.applier.Target(target)
	if !ok {
		return "", false
	}
	return orgOf(t), true
}

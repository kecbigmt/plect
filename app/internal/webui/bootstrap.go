package webui

import (
	"encoding/json"
	"net/http"
)

// bootstrapResponse is the React shell's own startup contract — intentionally
// separate from the generated Session contract in web/api (see that
// package's README "Authentication/bootstrap" note): it carries only the API
// version the client should expect compatibility with, and the CSRF token
// value, nothing about session data or the caller's identity.
type bootstrapResponse struct {
	APIVersion string `json:"apiVersion"`
	CSRFToken  string `json:"csrfToken"`
}

// handleBootstrap lets the shell learn its CSRF token: the plect_csrf cookie
// is HttpOnly, so JavaScript cannot read it directly and must be handed the
// same value here to echo back as X-CSRF-Token on a future mutation. This
// route is registered under /api/v1/ and sits behind the same
// authMiddleware/csrfMiddleware chain as every other route (server.go); an
// unauthenticated caller never reaches this handler, so it never mints or
// hands out a token to one.
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	tok := s.ensureCSRFToken(w, r)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(bootstrapResponse{APIVersion: "v1", CSRFToken: tok})
}

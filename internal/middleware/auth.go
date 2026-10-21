package middleware

import (
	"net/http"
	"strings"

	"marketplace/internal/api"
	"marketplace/internal/apperr"
	"marketplace/internal/auth"
)

// Auth пропускает /auth/* без токена, остальное только с валидным access токеном.
func Auth(tokens *auth.Tokens) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/auth/") {
				next.ServeHTTP(w, r)
				return
			}
			rawToken, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || rawToken == "" {
				apperr.Write(w, r, apperr.New(http.StatusUnauthorized, api.TOKENINVALID, "missing bearer token"))
				return
			}
			user, err := tokens.ParseAccess(rawToken)
			if err != nil {
				apperr.Write(w, r, err)
				return
			}
			setLogUserID(r.Context(), user.ID)
			next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), user)))
		})
	}
}

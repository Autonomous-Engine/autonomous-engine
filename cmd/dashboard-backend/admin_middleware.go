package main

import "net/http"

// AdminOnly is an HTTP middleware that restricts access to users whose JWT
// contains role == "admin". It must be applied after auth.Middleware so that
// the role value has already been placed in the request context.
func AdminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role, ok := r.Context().Value(contextKeyRole).(string)
		if !ok || role != "admin" {
			http.Error(w, `{"error":"admin access required"}`, http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

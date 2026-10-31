package internal

import (
	"context"
	"fmt"
	"net/http"
)

// Handler processes incoming HTTP requests.
type Handler struct {
	svc *Service
}

// NewHandler creates a new Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// ServeHTTP handles the request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	result, err := h.svc.Process(ctx, r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Fprint(w, result)
}

// Process does the actual work.
func (s *Service) Process(ctx context.Context, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	return "ok: " + path, nil
}

// Service is the business logic layer.
type Service struct{}

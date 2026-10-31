package items

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Server serves the item list.
type Server struct {
	items []string
}

// ListItems responds with every stored item.
func (s *Server) ListItems(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.items)
}

// CreateItem handles POST /items.
func (s *Server) CreateItem(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "name is required"})
		return
	}
	s.items = append(s.items, in.Name)
	w.WriteHeader(http.StatusCreated)
}

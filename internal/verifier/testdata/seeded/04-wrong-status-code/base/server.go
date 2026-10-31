package items

import (
	"encoding/json"
	"net/http"
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

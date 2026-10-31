package pkg

import "context"

// Repository defines the data access contract.
type Repository interface {
	FindByID(ctx context.Context, id string) (*Entity, error)
	Save(ctx context.Context, e *Entity) error
}

// Entity represents a domain object.
type Entity struct {
	ID   string
	Name string
}

// NewEntity creates a new Entity with the given name.
func NewEntity(name string) *Entity {
	return &Entity{Name: name}
}

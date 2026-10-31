package service

import "log"

// Service creates items.
type Service struct {
	store Store
	count int
}

// New returns a Service over store.
func New(store Store) *Service { return &Service{store: store} }

// Count is the number of items created.
func (s *Service) Count() int { return s.count }

// CreateItem saves name.
func (s *Service) CreateItem(name string) error {
	if name == "" {
		return nil
	}
	if err := s.store.Save(name); err != nil {
		log.Printf("save %s: %v", name, err)
	}
	s.count++
	return nil
}

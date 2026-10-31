package service

// Store persists items.
type Store interface {
	Save(name string) error
}

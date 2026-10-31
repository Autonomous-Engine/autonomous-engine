package service

import "testing"

type memStore struct{ names []string }

func (m *memStore) Save(name string) error {
	m.names = append(m.names, name)
	return nil
}

func TestCreateItem(t *testing.T) {
	st := &memStore{}
	s := New(st)
	if err := s.CreateItem("pen"); err != nil {
		t.Fatal(err)
	}
	if s.Count() != 1 || len(st.names) != 1 {
		t.Fatalf("count=%d names=%v", s.Count(), st.names)
	}
}

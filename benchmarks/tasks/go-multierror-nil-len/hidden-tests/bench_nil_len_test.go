package multierror

import (
	"errors"
	"sort"
	"testing"
)

// Held-out acceptance tests: Len must tolerate a nil *Error.

func TestBenchNilErrorLenIsZero(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Len() on a nil *Error panicked: %v", r)
		}
	}()
	var merr *Error
	if got := merr.Len(); got != 0 {
		t.Errorf("nil (*Error).Len() = %d, want 0", got)
	}
	if merr.ErrorOrNil() != nil {
		t.Errorf("nil (*Error).ErrorOrNil() must stay nil")
	}
}

func TestBenchLenCountsCollectedErrors(t *testing.T) {
	empty := &Error{}
	if got := empty.Len(); got != 0 {
		t.Errorf("empty Error.Len() = %d, want 0", got)
	}
	merr := &Error{Errors: []error{errors.New("b"), errors.New("a"), errors.New("c")}}
	if got := merr.Len(); got != 3 {
		t.Errorf("Len() = %d, want 3", got)
	}
	sort.Sort(merr)
	if merr.Errors[0].Error() != "a" || merr.Errors[2].Error() != "c" {
		t.Errorf("sort.Sort(*Error) did not order errors: %v", merr.Errors)
	}
	if got := merr.Len(); got != 3 {
		t.Errorf("Len() after sort = %d, want 3", got)
	}
}

func TestBenchAppendThenLen(t *testing.T) {
	var result error
	result = Append(result, errors.New("one"))
	result = Append(result, errors.New("two"))
	merr, ok := result.(*Error)
	if !ok {
		t.Fatalf("Append returned %T, want *Error", result)
	}
	if got := merr.Len(); got != 2 {
		t.Errorf("Len() after two Appends = %d, want 2", got)
	}
}

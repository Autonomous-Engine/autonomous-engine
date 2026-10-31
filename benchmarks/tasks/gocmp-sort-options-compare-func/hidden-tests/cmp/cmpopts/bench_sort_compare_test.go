package cmpopts_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// Held-out acceptance tests: SortSlices and SortMaps accept a three-way compare function
// "func(T, T) int" in addition to a less function "func(T, T) bool".

func benchOption(t *testing.T, what string, build func() cmp.Option) (opt cmp.Option) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v", what, r)
		}
	}()
	return build()
}

func benchIntCompare(x, y int) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

func TestBenchSortSlicesWithCompareFunc(t *testing.T) {
	opt := benchOption(t, "SortSlices(func(int, int) int)", func() cmp.Option {
		return cmpopts.SortSlices(benchIntCompare)
	})
	if !cmp.Equal([]int{3, 1, 2, 2}, []int{2, 1, 2, 3}, opt) {
		t.Errorf("slices with the same elements in different order are not Equal under SortSlices(compare)")
	}
	if cmp.Equal([]int{3, 1, 2}, []int{1, 2, 4}, opt) {
		t.Errorf("slices with different elements are Equal under SortSlices(compare)")
	}

	strOpt := benchOption(t, "SortSlices(strings.Compare)", func() cmp.Option {
		return cmpopts.SortSlices(strings.Compare)
	})
	got := struct{ Tags []string }{[]string{"b", "c", "a"}}
	want := struct{ Tags []string }{[]string{"a", "b", "c"}}
	if d := cmp.Diff(want, got, strOpt); d != "" {
		t.Errorf("nested slice not sorted with strings.Compare (-want +got):\n%s", d)
	}
}

func TestBenchSortSlicesCompareIsStable(t *testing.T) {
	// Compare by absolute value only: -1 and 1 tie, so their relative order is preserved.
	byAbs := func(x, y int) int {
		ax, ay := x, y
		if ax < 0 {
			ax = -ax
		}
		if ay < 0 {
			ay = -ay
		}
		return benchIntCompare(ax, ay)
	}
	opt := benchOption(t, "SortSlices(byAbs)", func() cmp.Option { return cmpopts.SortSlices(byAbs) })
	if !cmp.Equal([]int{2, -1, 1}, []int{-1, 1, 2}, opt) {
		t.Errorf("tied elements were reordered or sort did not apply")
	}
	if cmp.Equal([]int{-1, 1}, []int{1, -1}, opt) {
		t.Errorf("tied elements in different relative order compared Equal; the sort must be stable")
	}
}

func TestBenchSortMapsWithCompareFunc(t *testing.T) {
	opt := benchOption(t, "SortMaps(strings.Compare)", func() cmp.Option {
		return cmpopts.SortMaps(strings.Compare)
	})
	a := map[string]int{"x": 1, "y": 2, "z": 3}
	b := map[string]int{"z": 3, "x": 1, "y": 2}
	if !cmp.Equal(a, b, opt) {
		t.Errorf("equal maps are not Equal under SortMaps(compare)")
	}
	c := map[string]int{"x": 1, "y": 2, "z": 4}
	if cmp.Equal(a, c, opt) {
		t.Errorf("different maps are Equal under SortMaps(compare)")
	}
}

func TestBenchLessFuncsStillWork(t *testing.T) {
	opt := benchOption(t, "SortSlices(less)", func() cmp.Option {
		return cmpopts.SortSlices(func(x, y int) bool { return x < y })
	})
	if !cmp.Equal([]int{2, 1}, []int{1, 2}, opt) {
		t.Errorf("SortSlices with a less function no longer sorts")
	}
	mopt := benchOption(t, "SortMaps(less)", func() cmp.Option {
		return cmpopts.SortMaps(func(x, y string) bool { return x < y })
	})
	if !cmp.Equal(map[string]int{"a": 1}, map[string]int{"a": 1}, mopt) {
		t.Errorf("SortMaps with a less function no longer works")
	}
}

func TestBenchInvalidSortFuncsStillPanic(t *testing.T) {
	for name, fn := range map[string]interface{}{
		"func(int, int) string":  func(x, y int) string { return "" },
		"func(int, string) int":  func(x int, y string) int { return 0 },
		"func(int) int":          func(x int) int { return 0 },
		"nil func(int, int) int": (func(x, y int) int)(nil),
	} {
		for _, opt := range []struct {
			name  string
			build func(interface{}) cmp.Option
		}{{"SortSlices", cmpopts.SortSlices}, {"SortMaps", cmpopts.SortMaps}} {
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("%s(%s) did not panic", opt.name, name)
					}
				}()
				opt.build(fn)
			}()
		}
	}
}

package main

import (
	"reflect"
	"testing"
)

// The prefill counted the test-only helper package (internal/test) as a
// module and flagged it high-risk because every test file imports it
// ("8 of 10" when the codex held 9; "enmime, test" as high-risk) (rerun
// NEW-4). The codex skips test-only modules; the prefill's module count,
// names and risk heuristic must see the same source modules.
func TestPrefillModuleListsSkipTestOnlyModules(t *testing.T) {
	modules := []moduleInfo{
		{Name: "enmime", Path: "."},
		{Name: "test", Path: "internal/test", IsTest: true, Imports: []string{"github.com/x/enmime"}},
		{Name: "coding", Path: "internal/coding"},
	}
	// Six source modules import enmime, seven test-only modules import test.
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		modules = append(modules, moduleInfo{Name: n, Path: "cmd/" + n, Imports: []string{"github.com/x/enmime"}})
	}
	for _, n := range []string{"a_test", "b_test", "c_test", "d_test", "e_test", "f_test", "g_test"} {
		modules = append(modules, moduleInfo{Name: n, Path: "t/" + n, IsTest: true, Imports: []string{"github.com/x/enmime/internal/test"}})
	}

	names, highRisk := prefillModuleLists(modules)
	wantNames := []string{"enmime", "coding", "a", "b", "c", "d", "e", "f"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("module names = %v, want the source modules %v", names, wantNames)
	}
	if !reflect.DeepEqual(highRisk, []string{"enmime"}) {
		t.Errorf("high-risk = %v, want [enmime] only (test-only modules neither count nor are counted)", highRisk)
	}
}

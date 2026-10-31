package plan

import (
	"strings"
	"testing"
)

// one_sided_contract (M2-06): both sides changed, or neither, is fine; one
// side only is rejected, naming the missing side. Repositories match by id
// or by name.
func TestOneSidedContract(t *testing.T) {
	brief := BriefCriteria{SchemaVersion: SchemaVersion, Criteria: []Criterion{{
		ID: "AC-1", Text: "t", Source: Source{Kind: SourceUser, Quote: "q"}, Risk: RiskLow,
		Contracts: []ContractRef{
			{Type: "event", Name: "user.created", ProducerRepository: "repo-api", ConsumerRepository: "Web"},
			// Same repository on both sides, or one side missing: not an edge.
			{Type: "rest", Name: "GET /x", ProducerRepository: "repo-api", ConsumerRepository: "repo-api"},
			{Type: "rest", Name: "GET /y", ProducerRepository: "repo-api"},
		},
	}}}
	task := func(i int, repoID, repo string) TaskSpecWithFiles {
		return TaskSpecWithFiles{Index: i, Title: "t", RepositoryID: repoID, Repository: repo}
	}
	cases := []struct {
		name  string
		tasks []TaskSpecWithFiles
		want  string
	}{
		{"both sides", []TaskSpecWithFiles{task(1, "repo-api", "api"), task(2, "repo-web", "web")}, ""},
		{"neither side", []TaskSpecWithFiles{task(1, "repo-docs", "docs")}, ""},
		{"consumer only", []TaskSpecWithFiles{task(2, "repo-web", "web")}, "changed only on the consumer side (repository Web: task 2 \"t\"); no task changes the producer repository repo-api"},
		{"producer only", []TaskSpecWithFiles{task(1, "repo-api", "api")}, "changed only on the producer side"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := oneSidedContracts(brief, tc.tasks)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("violations = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Code != CodeOneSidedContract || !strings.Contains(got[0].Message, tc.want) {
				t.Fatalf("violations = %+v, want one containing %q", got, tc.want)
			}
		})
	}
}

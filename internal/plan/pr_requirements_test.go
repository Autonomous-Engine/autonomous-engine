package plan

import (
	"strconv"
	"strings"
	"testing"
)

// R15 T2: the intake's sentences that ask for something to be stated in
// the PR text, verbatim, in order, without restatements.
func TestPRTextRequirementsFromTheIntake(t *testing.T) {
	intake := "Symptom: tiles fail.\r\n\r\nGoals:\nA. Add a route. Open a pull request when done.\n" +
		"Hard constraints: never invent a key. This change cannot fix it alone: the pull request description MUST state explicitly that the owner must set TILES_API_KEY (.env / docker-compose) for the error to go away. `npm test` must keep passing."
	prd := "## Requirements\n6. **Pull Request Description**:\n   - The pull request description explicitly states that the owner must set `TILES_API_KEY` for the error to go away.\n" +
		"- The commit message mentions the ticket id.\n"
	got := PRTextRequirements(intake, prd)
	want := []string{
		"This change cannot fix it alone: the pull request description MUST state explicitly that the owner must set TILES_API_KEY (.env / docker-compose) for the error to go away.",
		"The commit message mentions the ticket id.",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if PRTextRequirements("", "No PR text here. Open a pull request.") != nil {
		t.Error("no PR-text sentence: want none")
	}

	var many []string
	for i := 0; i < 25; i++ {
		n := strconv.Itoa(i)
		many = append(many, "Item "+n+": say in the PR description that key"+n+" and var"+n+" and flag"+n+" are set.")
	}
	if n := len(PRTextRequirements(strings.Join(many, "\n"))); n != MaxPRRequirements {
		t.Errorf("%d requirements, want the cap %d", n, MaxPRRequirements)
	}
}

func TestRestatesRequirement(t *testing.T) {
	a := "Explicitly state in the pull request description that the repository owner must obtain a TileHost API key."
	b := "The pull request description explicitly states that the repository owner must obtain a TileHost API key and set it."
	if !RestatesRequirement(a, b) || !RestatesRequirement(b, a) {
		t.Error("restatement not recognised")
	}
	if RestatesRequirement("The PR description must list the new environment variables.", a) {
		t.Error("different requirement taken for a restatement")
	}
	if RestatesRequirement("", a) {
		t.Error("empty text restates")
	}
}

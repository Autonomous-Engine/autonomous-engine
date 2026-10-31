package main

import "testing"

// R3 review: research output cannot mark its own criteria non-code (that
// would exempt them from plan ownership); the parser strips the flag.
func TestParseCriteriaOutputStripsNonCode(t *testing.T) {
	bc, err := parseCriteriaOutput(`{"schema_version":1,"criteria":[
		{"id":"AC-1","text":"Docs describe the key","source":{"kind":"user","quote":"document the key"},"risk":"low",
		 "non_code":true,"non_code_reason":"the model says so"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if c := bc.Criteria[0]; c.NonCode || c.NonCodeReason != "" {
		t.Fatalf("parsed criterion kept the model's non-code mark: %+v", c)
	}
}

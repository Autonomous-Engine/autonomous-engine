package llm

import (
	"errors"
	"testing"
)

// Ollama answers a model's malformed tool-call XML with a 500 ("XML syntax
// error on line 17: element <parameter> closed by </function>"). That is
// the model misfiring on one call, not the service failing: the chat agent
// treated it as fatal and the user's request produced nothing (rerun
// NEW-11). It is transient, so the call is retried.
func TestMalformedToolCallOutputIsTransient(t *testing.T) {
	err := errors.New(`ollama: status 500: {"error":"XML syntax error on line 17: element <parameter> closed by </function>"}`)
	if !IsTransientLLMError(err) {
		t.Fatal("a 500 for malformed tool-call output must be transient")
	}
	if IsTransientLLMError(errors.New("ollama: status 400: invalid model")) {
		t.Fatal("a 400 must not be transient")
	}
}

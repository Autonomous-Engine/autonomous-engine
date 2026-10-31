package auth

import "testing"

func TestAuthorizeServiceToken(t *testing.T) {
	tokens := Tokens{"svc_cron": "service:cron", "u1": "alice"}
	got, err := tokens.Authorize("Bearer svc_cron")
	if err != nil || got != "service:cron" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := tokens.Authorize("Bearer nope"); err != ErrUnauthorized {
		t.Fatalf("unknown user token: %v", err)
	}
}

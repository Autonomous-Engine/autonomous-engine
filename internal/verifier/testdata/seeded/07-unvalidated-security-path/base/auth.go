package auth

import (
	"errors"
	"strings"
)

// ErrUnauthorized rejects a request.
var ErrUnauthorized = errors.New("unauthorized")

// Tokens maps issued API tokens to the account they belong to.
type Tokens map[string]string

// Authorize returns the account of an "Authorization: Bearer <token>" header.
func (t Tokens) Authorize(header string) (string, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", ErrUnauthorized
	}
	token := strings.TrimPrefix(header, prefix)
	account, ok := t[token]
	if !ok {
		return "", ErrUnauthorized
	}
	return account, nil
}

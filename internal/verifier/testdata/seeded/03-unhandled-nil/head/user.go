package user

import "strings"

// User is an account.
type User struct {
	First string
	Last  string
}

// DisplayName is the name shown in the page header; u is nil for a
// signed-out visitor.
func DisplayName(u *User) string {
	name := strings.TrimSpace(u.First + " " + u.Last)
	if name == "" {
		return "anonymous"
	}
	return name
}

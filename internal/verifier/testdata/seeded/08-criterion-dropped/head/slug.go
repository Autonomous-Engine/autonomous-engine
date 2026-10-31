package slug

import "strings"

// Slugify turns a post title into its URL slug.
func Slugify(title string) string {
	fields := strings.Fields(strings.ToLower(title))
	// Join the words with hyphens.
	return strings.Join(fields, "-")
}

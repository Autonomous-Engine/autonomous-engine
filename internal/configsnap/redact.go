package configsnap

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Redacted replaces every value a snapshot must not carry. The key stays so
// a diff still shows that a credential is configured; its value (and so a
// key rotation) never reaches the document or its hash.
const Redacted = "[redacted]"

var (
	camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	nonAlnum      = regexp.MustCompile(`[^a-z0-9]+`)
)

// keyWords splits a settings key into lowercase words: "apiKey",
// "api_key", "API-Key" all become [api key].
func keyWords(k string) []string {
	k = camelBoundary.ReplaceAllString(k, "${1} ${2}")
	return strings.Fields(nonAlnum.ReplaceAllString(strings.ToLower(k), " "))
}

// IsSecretKey reports whether a settings key names a credential. It is
// deliberately word-based, not substring-based: "max_tokens" or
// "keep_alive" shape behaviour and must stay in the snapshot, while
// "api_key", "apiKey", "client_secret", "access_token", "password" and
// "api_key_credential_id" must not.
func IsSecretKey(k string) bool {
	words := keyWords(k)
	for i, w := range words {
		switch w {
		case "secret", "secrets", "password", "passwd", "passphrase", "credential", "credentials",
			"apikey", "bearer", "authorization", "cookie", "privatekey":
			return true
		case "key":
			if i > 0 {
				switch words[i-1] {
				case "api", "private", "access", "secret", "signing", "encryption", "master":
					return true
				}
			}
		case "token":
			if len(words) == 1 {
				return true
			}
			if i > 0 {
				switch words[i-1] {
				case "access", "auth", "api", "refresh", "bearer", "session", "id":
					return true
				}
			}
		}
	}
	return false
}

// Redact returns a copy of v (a decoded JSON tree) with every secret-keyed
// value replaced by Redacted and every URL-valued string stripped of user
// info and secret query parameters.
func Redact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if IsSecretKey(k) {
				if val == nil {
					out[k] = nil
				} else {
					out[k] = Redacted
				}
				continue
			}
			out[k] = Redact(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = Redact(val)
		}
		return out
	case string:
		return RedactURL(t)
	default:
		return v
	}
}

// RedactURL strips user info and secret query parameters from s when s is
// an absolute URL; any other string is returned unchanged.
func RedactURL(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return s
	}
	changed := false
	if u.User != nil {
		u.User = nil
		changed = true
	}
	if u.RawQuery != "" {
		q := u.Query()
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if IsSecretKey(k) || strings.EqualFold(k, "key") || strings.EqualFold(k, "sig") {
				q.Set(k, Redacted)
				changed = true
			}
		}
		if changed {
			u.RawQuery = q.Encode()
		}
	}
	if !changed {
		return s
	}
	return u.String()
}

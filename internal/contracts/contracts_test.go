package contracts

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tree writes files (slash path -> content) under a fresh directory.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func openAPI(fields ...string) string {
	props := make([]string, 0, len(fields))
	for _, f := range fields {
		props = append(props, `"`+f+`": {"type": "string"}`)
	}
	return `{
  "openapi": "3.0.0",
  "paths": {
    "/api/users/{id}": {
      "get": {
        "responses": {
          "200": {
            "content": {"application/json": {"schema": {"$ref": "#/components/schemas/User"}}}
          }
        }
      }
    }
  },
  "components": {"schemas": {"User": {"type": "object", "properties": {` + strings.Join(props, ", ") + `}}}}
}`
}

const restConsumer = `import { api } from './api'

export async function loadUser(id: string) {
  const res = await fetch(` + "`/api/users/${id}`" + `)
  const body = await res.json()
  return { name: body.name, contact: body.email }
}
`

const eventConsts = "package events\n\nconst TopicUserCreated = \"user.created\"\n"

func userPayload(emailTag string, extra string) string {
	return "package events\n\ntype UserCreatedPayload struct {\n\tUserID string `json:\"user_id\"`\n\tEmail  string `json:\"" + emailTag + "\"`\n" + extra + "}\n"
}

const eventConsumer = `package worker

type userCreated struct {
	UserID string ` + "`json:\"user_id\"`" + `
	Email  string ` + "`json:\"email\"`" + `
}

func (w *Worker) Start() error {
	return w.bus.Subscribe("user.created", w.onUserCreated)
}
`

const usersTable = `CREATE TABLE IF NOT EXISTS users (
    id UUID PRIMARY KEY,
    email TEXT NOT NULL,
    legacy_email TEXT
);
`

const dbConsumer = `package store

const getUserSQL = ` + "`" + `
	SELECT id, email, legacy_email
	  FROM users
	 WHERE id = $1` + "`" + `
`

func TestCheck(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		ref        Ref
		base, head map[string]string
		consumer   map[string]string
		want       Status
		// wantText must all appear in the result's Summary.
		wantText []string
	}{
		{
			name:     "rest: a removed response field the consumer reads",
			ref:      Ref{Type: "rest", Name: "GET /api/users/{id}"},
			base:     map[string]string{"api/openapi.json": openAPI("id", "name", "email")},
			head:     map[string]string{"api/openapi.json": openAPI("id", "name")},
			consumer: map[string]string{"src/user.ts": restConsumer},
			want:     StatusMismatch,
			wantText: []string{"removed field email from the GET /api/users/{id} response (api/openapi.json)", "src/user.ts:6:", "body.email"},
		},
		{
			name:     "rest: an added response field is compatible",
			ref:      Ref{Type: "rest_openapi", Name: "GET /api/users/{id}"},
			base:     map[string]string{"api/openapi.json": openAPI("id", "name", "email")},
			head:     map[string]string{"api/openapi.json": openAPI("id", "name", "email", "avatar")},
			consumer: map[string]string{"src/user.ts": restConsumer},
			want:     StatusOK,
		},
		{
			name:     "rest: a removed field the consumer does not read is compatible",
			ref:      Ref{Type: "rest", Name: "GET /api/users/{id}"},
			base:     map[string]string{"api/openapi.json": openAPI("id", "name", "email", "legacy")},
			head:     map[string]string{"api/openapi.json": openAPI("id", "name", "email")},
			consumer: map[string]string{"src/user.ts": restConsumer},
			want:     StatusOK,
		},
		{
			name:     "rest: a removed route the consumer calls (route table only)",
			ref:      Ref{Type: "rest", Name: "GET /api/orders"},
			base:     map[string]string{"server/routes.go": "package server\n\nfunc routes(r *Router) {\n\tr.GET(\"/api/orders\", listOrders)\n}\n"},
			head:     map[string]string{"server/routes.go": "package server\n\nfunc routes(r *Router) {\n}\n"},
			consumer: map[string]string{"web/orders.js": "export const list = () => fetch('/api/orders').then(r => r.json())\n"},
			want:     StatusMismatch,
			wantText: []string{"removed GET /api/orders", "web/orders.js:1:"},
		},
		{
			name:     "rest: route kept but no schema to compare fields is unchecked, not a pass",
			ref:      Ref{Type: "rest", Name: "GET /api/orders"},
			base:     map[string]string{"server/routes.go": "package server\n\nfunc routes(r *Router) {\n\tr.GET(\"/api/orders\", listOrders)\n}\n"},
			head:     map[string]string{"server/routes.go": "package server\n\nfunc routes(r *Router) {\n\tr.GET(\"/api/orders\", listOrdersV2)\n}\n"},
			consumer: map[string]string{"web/orders.js": "export const list = () => fetch('/api/orders')\n"},
			want:     StatusUnchecked,
			wantText: []string{"no OpenAPI JSON response schema"},
		},
		{
			name:     "event: a renamed payload field the consumer decodes",
			ref:      Ref{Type: "events", Name: "TopicUserCreated"},
			base:     map[string]string{"internal/events/topics.go": eventConsts, "internal/events/user.go": userPayload("email", "")},
			head:     map[string]string{"internal/events/topics.go": eventConsts, "internal/events/user.go": userPayload("email_address", "")},
			consumer: map[string]string{"worker/user.go": eventConsumer},
			want:     StatusMismatch,
			wantText: []string{"renamed field email to email_address in the TopicUserCreated payload", "worker/user.go:5:"},
		},
		{
			name:     "event: an added payload field is compatible",
			ref:      Ref{Type: "event", Name: "user.created"},
			base:     map[string]string{"internal/events/topics.go": eventConsts, "internal/events/user.go": userPayload("email", "")},
			head:     map[string]string{"internal/events/topics.go": eventConsts, "internal/events/user.go": userPayload("email", "\tName string `json:\"name\"`\n")},
			consumer: map[string]string{"worker/user.go": eventConsumer},
			want:     StatusOK,
		},
		{
			name:     "event: a JSON Schema payload field removal",
			ref:      Ref{Type: "event", Name: "order.shipped"},
			base:     map[string]string{"schemas/order-shipped.schema.json": `{"title":"OrderShipped","properties":{"order_id":{"type":"string"},"carrier":{"type":"string"}}}`},
			head:     map[string]string{"schemas/order-shipped.schema.json": `{"title":"OrderShipped","properties":{"order_id":{"type":"string"}}}`},
			consumer: map[string]string{"src/ship.ts": "bus.on('order.shipped', (e) => notify(e.order_id, e.carrier))\n"},
			want:     StatusMismatch,
			wantText: []string{"removed field carrier", "src/ship.ts:1:"},
		},
		{
			name:     "event: no payload definition is unchecked",
			ref:      Ref{Type: "event", Name: "order.cancelled"},
			base:     map[string]string{"README.md": "nothing\n"},
			head:     map[string]string{"README.md": "nothing\n"},
			consumer: map[string]string{"src/x.ts": "bus.on('order.cancelled', f)\n"},
			want:     StatusUnchecked,
			wantText: []string{"no payload definition"},
		},
		{
			name:     "db_schema: a dropped column the consumer selects",
			ref:      Ref{Type: "sql_schemas", Name: "users"},
			base:     map[string]string{"migrations/001_users.up.sql": usersTable},
			head:     map[string]string{"migrations/001_users.up.sql": usersTable, "migrations/002_drop_legacy.up.sql": "ALTER TABLE users DROP COLUMN IF EXISTS legacy_email;\n", "migrations/002_drop_legacy.down.sql": "ALTER TABLE users ADD COLUMN IF NOT EXISTS legacy_email TEXT;\n"},
			consumer: map[string]string{"internal/store/users.go": dbConsumer},
			want:     StatusMismatch,
			wantText: []string{"dropped column users.legacy_email (migrations/002_drop_legacy.up.sql)", "internal/store/users.go:4:"},
		},
		{
			name:     "db_schema: an added column is compatible (and a down migration's drop is not a change)",
			ref:      Ref{Type: "db_schema", Name: "users"},
			base:     map[string]string{"migrations/001_users.up.sql": usersTable},
			head:     map[string]string{"migrations/001_users.up.sql": usersTable, "migrations/002_nickname.up.sql": "ALTER TABLE users ADD COLUMN IF NOT EXISTS nickname TEXT;\n", "migrations/002_nickname.down.sql": "ALTER TABLE users DROP COLUMN IF EXISTS nickname;\n"},
			consumer: map[string]string{"internal/store/users.go": dbConsumer},
			want:     StatusOK,
		},
		{
			name:     "db_schema: a renamed column the consumer selects",
			ref:      Ref{Type: "db_schema", Name: "users.legacy_email"},
			base:     map[string]string{"migrations/001_users.up.sql": usersTable},
			head:     map[string]string{"migrations/001_users.up.sql": usersTable, "migrations/002_rename.up.sql": "ALTER TABLE users RENAME COLUMN legacy_email TO old_email;\n"},
			consumer: map[string]string{"internal/store/users.go": dbConsumer},
			want:     StatusMismatch,
			wantText: []string{"renamed column users.legacy_email to old_email"},
		},
		{
			name:     "db_schema: a producer without SQL is unchecked",
			ref:      Ref{Type: "db_schema", Name: "users"},
			base:     map[string]string{"main.go": "package main\n"},
			head:     map[string]string{"main.go": "package main\n"},
			consumer: map[string]string{"internal/store/users.go": dbConsumer},
			want:     StatusUnchecked,
		},
		{
			name:     "rest: a consumer head that does not call the path is unchecked, not ok",
			ref:      Ref{Type: "rest", Name: "GET /api/users/{id}"},
			base:     map[string]string{"api/openapi.json": openAPI("id", "name", "email")},
			head:     map[string]string{"api/openapi.json": openAPI("id", "name")},
			consumer: map[string]string{"src/other.ts": "export const x = 1\n"},
			want:     StatusUnchecked,
			wantText: []string{"no call of /api/users/{id} found in the consumer head"},
		},
		{
			name:     "rest: the codex consumer file is checked directly when the call is built dynamically",
			ref:      Ref{Type: "rest", Name: "GET /api/users/{id}", ConsumerFile: "src/user.ts", ConsumerLine: 2},
			base:     map[string]string{"api/openapi.json": openAPI("id", "name", "email")},
			head:     map[string]string{"api/openapi.json": openAPI("id", "name")},
			consumer: map[string]string{"src/user.ts": "export async function load(id: string) {\n  const res = await http(['api', 'users', id].join('/'))\n  return (await res.json()).email\n}\n"},
			want:     StatusMismatch,
			wantText: []string{"removed field email", "src/user.ts:3:"},
		},
		{
			name:     "event: a package-qualified constant is a decoding site",
			ref:      Ref{Type: "event", Name: "TopicUserCreated"},
			base:     map[string]string{"internal/events/topics.go": eventConsts, "internal/events/user.go": userPayload("email", "")},
			head:     map[string]string{"internal/events/topics.go": eventConsts, "internal/events/user.go": userPayload("email_address", "")},
			consumer: map[string]string{"worker/user.go": "package worker\n\nfunc (w *Worker) Start() error {\n\treturn w.bus.Subscribe(events.TopicUserCreated, func(m map[string]any) { notify(m[\"email\"]) })\n}\n"},
			want:     StatusMismatch,
			wantText: []string{"renamed field email to email_address", "worker/user.go:4:"},
		},
		{
			name:     "event: no decoding site in the consumer is unchecked, not ok",
			ref:      Ref{Type: "event", Name: "TopicUserCreated"},
			base:     map[string]string{"internal/events/topics.go": eventConsts, "internal/events/user.go": userPayload("email", "")},
			head:     map[string]string{"internal/events/topics.go": eventConsts, "internal/events/user.go": userPayload("email_address", "")},
			consumer: map[string]string{"worker/other.go": "package worker\n"},
			want:     StatusUnchecked,
			wantText: []string{"no decoding site of the event was found"},
		},
		{
			name:     "db_schema: a table the producer's SQL does not define is unchecked, not ok",
			ref:      Ref{Type: "db_schema", Name: "orders"},
			base:     map[string]string{"migrations/001_users.up.sql": usersTable},
			head:     map[string]string{"migrations/001_users.up.sql": usersTable, "migrations/002_nickname.up.sql": "ALTER TABLE users ADD COLUMN IF NOT EXISTS nickname TEXT;\n"},
			consumer: map[string]string{"internal/store/orders.go": "package store\n\nconst q = `SELECT id FROM orders`\n"},
			want:     StatusUnchecked,
			wantText: []string{"defines no table orders"},
		},
		{
			name:     "shared_package has no checker",
			ref:      Ref{Type: "shared_packages", Name: "@acme/ui"},
			base:     map[string]string{"package.json": "{}"},
			head:     map[string]string{"package.json": "{}"},
			consumer: map[string]string{"package.json": "{}"},
			want:     StatusUnchecked,
			wantText: []string{"no checker for contract type"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			producer := Side{Base: tree(t, tc.base), Head: tree(t, tc.head)}
			consumer := Side{Head: tree(t, tc.consumer)}
			got := Check(ctx, tc.ref, producer, consumer)
			if got.Status != tc.want {
				t.Fatalf("status = %s, want %s: %s", got.Status, tc.want, got.Summary())
			}
			s := got.Summary()
			for _, w := range tc.wantText {
				if !strings.Contains(s, w) {
					t.Errorf("summary lacks %q:\n%s", w, s)
				}
			}
		})
	}
}

// An unpinned producer has no base to diff: unchecked, never ok.
func TestCheck_UnpinnedProducerIsUnchecked(t *testing.T) {
	head := tree(t, map[string]string{"api/openapi.json": openAPI("id")})
	got := Check(context.Background(), Ref{Type: "rest", Name: "GET /api/users/{id}"}, Side{Head: head}, Side{Head: tree(t, map[string]string{"a.ts": restConsumer})})
	if got.Status != StatusUnchecked || !strings.Contains(got.Summary(), "no pinned base") {
		t.Fatalf("got %s: %s", got.Status, got.Summary())
	}
}

func TestNormalizeType(t *testing.T) {
	for in, want := range map[string]string{
		"rest": TypeREST, "rest_openapi": TypeREST, "rest.route": TypeREST,
		"events": TypeEvent, "event.topic": TypeEvent,
		"sql_schemas": TypeDBSchema, "sql.table": TypeDBSchema, "db_schema": TypeDBSchema,
		"shared_packages": TypeSharedPackage, "go_interfaces": TypeSharedPackage, "ts_exports": TypeSharedPackage,
		"graphql": TypeOther, "grpc_protobuf": TypeOther, "": TypeOther,
	} {
		if got := NormalizeType(in); got != want {
			t.Errorf("NormalizeType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConsumerPathPattern(t *testing.T) {
	re := consumerPathPattern("/api/users/{id}")
	for _, l := range []string{"fetch(`/api/users/${id}`)", "axios.get('/api/users/' + id)", `get("/api/users/42")`, "fetch(`${BASE}/api/users/${id}`)"} {
		if !re.MatchString(l) {
			t.Errorf("%q does not match", l)
		}
	}
	for _, l := range []string{"fetch('/api/users/1/orders')", "fetch('/api/userset')"} {
		if re.MatchString(l) {
			t.Errorf("%q matches", l)
		}
	}
}

func TestNormalizeName(t *testing.T) {
	for _, s := range []string{"user.created", "TopicUserCreated", "UserCreatedPayload", "user-created.schema", "EventUserCreated"} {
		if got := normalizeName(s); got != "usercreated" {
			t.Errorf("normalizeName(%q) = %q", s, got)
		}
	}
}

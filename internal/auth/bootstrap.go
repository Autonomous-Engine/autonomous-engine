package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// Bootstrap performs first-run initialisation. If the users table already
// contains at least one row it returns immediately without making any changes.
//
// When the table is empty it:
//  1. Generates a random 16-character hex password.
//  2. Hashes it with bcrypt.
//  3. Inserts an admin user (admin@localhost) with password_change_required=true.
//  4. Inserts the local auth provider as the primary provider.
//  5. Prints the generated credentials to stdout in a clearly visible banner.
func Bootstrap(ctx context.Context, db *pgxpool.Pool) error {
	var count int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return fmt.Errorf("bootstrap: count users: %w", err)
	}
	if count > 0 {
		return nil
	}

	// Generate a random 16-character hex password (8 random bytes → 16 hex chars).
	rawBytes := make([]byte, 8)
	if _, err := rand.Read(rawBytes); err != nil {
		return fmt.Errorf("bootstrap: generate password: %w", err)
	}
	password := hex.EncodeToString(rawBytes)

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("bootstrap: hash password: %w", err)
	}

	_, err = db.Exec(ctx,
		`INSERT INTO users
		       (email, name, role, password_hash, password_change_required, auth_provider)
		 VALUES ('admin@localhost', 'System Admin', 'admin', $1, true, 'local')`,
		string(hash),
	)
	if err != nil {
		return fmt.Errorf("bootstrap: insert admin user: %w", err)
	}

	_, err = db.Exec(ctx,
		`INSERT INTO auth_providers (provider_type, name, enabled, is_primary)
		 VALUES ('local', 'Local Authentication', true, true)
		 ON CONFLICT (provider_type) DO NOTHING`,
	)
	if err != nil {
		return fmt.Errorf("bootstrap: insert auth_providers: %w", err)
	}

	log.Println("╔══════════════════════════════════════════════════════╗")
	log.Println("║           FIRST-RUN ADMIN CREDENTIALS                ║")
	log.Printf( "║  Email   : admin@localhost                           ║")
	log.Printf( "║  Password: %-40s  ║", password)
	log.Println("║  You will be prompted to change this password.       ║")
	log.Println("╚══════════════════════════════════════════════════════╝")

	return nil
}

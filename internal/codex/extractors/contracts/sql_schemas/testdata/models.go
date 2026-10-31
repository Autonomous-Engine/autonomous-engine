package models

import "database/sql"

func GetUser(db *sql.DB, id string) error {
	query := "SELECT id, name, email FROM users WHERE id = $1"
	return db.QueryRow(query, id).Scan()
}

func ListOrders(db *sql.DB, userID string) error {
	query := "SELECT id, total FROM orders WHERE user_id = $1"
	return db.QueryRow(query, userID).Scan()
}

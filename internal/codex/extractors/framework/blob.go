// internal/codex/extractors/framework/blob.go
package framework

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type BlobStore struct {
	db *pgxpool.Pool
}

func NewBlobStore(db *pgxpool.Pool) *BlobStore { return &BlobStore{db: db} }

func (s *BlobStore) Insert(ctx context.Context, b *Blob, extractor string) error {
	b.Hash = CanonicalHash(b.Payload)
	_, err := s.db.Exec(ctx, `
		INSERT INTO codex_blobs (hash, section, extractor, payload, confidence)
		VALUES ($1, $2, $3, $4::jsonb, $5)
		ON CONFLICT (hash) DO NOTHING
	`, b.Hash, b.Section, extractor, b.Payload, b.Confidence)
	return err
}

func (s *BlobStore) Get(ctx context.Context, hash string) (*Blob, error) {
	b := &Blob{Hash: hash}
	var payload []byte
	err := s.db.QueryRow(ctx, `
		SELECT section, payload, confidence FROM codex_blobs WHERE hash = $1
	`, hash).Scan(&b.Section, &payload, &b.Confidence)
	if err != nil {
		return nil, err
	}
	b.Payload = payload
	return b, nil
}

func (s *BlobStore) GetMany(ctx context.Context, hashes []string) ([]*Blob, map[string]string, error) {
	if len(hashes) == 0 {
		return nil, nil, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT hash, section, extractor, payload, confidence FROM codex_blobs WHERE hash = ANY($1)
	`, hashes)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	blobs := make([]*Blob, 0, len(hashes))
	extractors := make(map[string]string, len(hashes))
	for rows.Next() {
		b := &Blob{}
		var payload []byte
		var extractor string
		if err := rows.Scan(&b.Hash, &b.Section, &extractor, &payload, &b.Confidence); err != nil {
			return nil, nil, err
		}
		b.Payload = payload
		blobs = append(blobs, b)
		extractors[b.Hash] = extractor
	}
	return blobs, extractors, rows.Err()
}

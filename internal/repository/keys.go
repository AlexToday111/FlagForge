package repository

import (
	"context"
	"time"
)

type KeyInfo struct {
	ID        string     `json:"id"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

func (r *Repository) SDKKeys(ctx context.Context, p Principal, project, envKey string) ([]KeyInfo, error) {
	if p.Role != "Admin" {
		return nil, ErrForbidden
	}
	e, err := environment(ctx, r.Pool, project, envKey, false)
	if err != nil {
		return nil, err
	}
	rows, err := r.Pool.Query(ctx, `SELECT id::text,created_at,revoked_at FROM sdk_keys WHERE environment_id=$1 ORDER BY created_at`, e.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KeyInfo{}
	for rows.Next() {
		var k KeyInfo
		if err = rows.Scan(&k.ID, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Schema is embedded with the binary so an installation never depends on cwd.
//
//go:embed schema.sql
var migrations embed.FS

type Store struct{ Pool *pgxpool.Pool }

func Open(ctx context.Context, url string) (*Store, error) {
	p, e := pgxpool.New(ctx, url)
	if e != nil {
		return nil, e
	}
	if e = p.Ping(ctx); e != nil {
		p.Close()
		return nil, e
	}
	schema, _ := migrations.ReadFile("schema.sql")
	conn, e := p.Acquire(ctx)
	if e != nil {
		p.Close()
		return nil, e
	}
	if _, e = conn.Exec(ctx, "SELECT pg_advisory_lock(628119)"); e != nil {
		conn.Release()
		p.Close()
		return nil, e
	}
	_, e = conn.Exec(ctx, string(schema))
	unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, unlockErr := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock(628119)")
	cancel()
	if unlockErr != nil {
		_ = conn.Conn().Close(context.Background())
	}
	conn.Release()
	if e != nil {
		p.Close()
		return nil, e
	}
	if unlockErr != nil {
		p.Close()
		return nil, unlockErr
	}
	return &Store{Pool: p}, nil
}
func (s *Store) Close() { s.Pool.Close() }
func (s *Store) Put(ctx context.Context, kind, id string, value any) error {
	b, e := json.Marshal(value)
	if e != nil {
		return e
	}
	_, e = s.Pool.Exec(ctx, `INSERT INTO mp_entities(kind,id,payload) VALUES($1,$2,$3) ON CONFLICT(kind,id) DO UPDATE SET payload=EXCLUDED.payload`, kind, id, b)
	return e
}
func (s *Store) Get(ctx context.Context, kind, id string, dest any) error {
	var b []byte
	e := s.Pool.QueryRow(ctx, `SELECT payload FROM mp_entities WHERE kind=$1 AND id=$2`, kind, id).Scan(&b)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, dest)
}
func (s *Store) Delete(ctx context.Context, kind, id string) error {
	_, e := s.Pool.Exec(ctx, `DELETE FROM mp_entities WHERE kind=$1 AND id=$2`, kind, id)
	return e
}
func (s *Store) List(ctx context.Context, kind string, dest any) error {
	rows, e := s.Pool.Query(ctx, `SELECT payload FROM mp_entities WHERE kind=$1 ORDER BY created_at DESC LIMIT 2000`, kind)
	if e != nil {
		return e
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			return e
		}
		out = append(out, b)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	b, e := json.Marshal(out)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, dest)
}
func IsMissing(e error) bool { return errors.Is(e, pgx.ErrNoRows) }
func PutTx(ctx context.Context, tx pgx.Tx, kind, id string, value any) error {
	b, e := json.Marshal(value)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO mp_entities(kind,id,payload) VALUES($1,$2,$3) ON CONFLICT(kind,id) DO UPDATE SET payload=EXCLUDED.payload`, kind, id, b)
	return e
}
func Marshal(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(fmt.Sprintf("marshal internal value: %v", e))
	}
	return b
}

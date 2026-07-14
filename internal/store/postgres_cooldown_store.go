package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

var _ cliproxyauth.CooldownStateStore = (*PostgresCooldownStateStore)(nil)

// PostgresCooldownStateStore persists runtime cooldown state to PostgreSQL,
// independently from PostgresStore's auth token storage (which owns Save
// for auth records; a separate type avoids a method name collision).
type PostgresCooldownStateStore struct {
	s *PostgresStore
}

// NewPostgresCooldownStateStore wraps an existing PostgresStore to provide
// cooldown state persistence via the cooldown_store table.
// Returns a true interface nil when s is nil, avoiding the non-nil interface
// holding a nil pointer trap.
func NewPostgresCooldownStateStore(s *PostgresStore) cliproxyauth.CooldownStateStore {
	if s == nil {
		return nil
	}
	return &PostgresCooldownStateStore{s: s}
}

// cooldownStateEnvelope is the JSON content stored per-row, grouped by AuthID.
type cooldownStateEnvelope struct {
	Version   int                                `json:"version"`
	AuthID    string                             `json:"auth_id,omitempty"`
	Provider  string                             `json:"provider,omitempty"`
	UpdatedAt time.Time                          `json:"updated_at"`
	Records   []cliproxyauth.CooldownStateRecord `json:"records"`
}

// groupCooldownRecordsByAuthID buckets records by trimmed AuthID, skipping
// records with an empty AuthID.
func groupCooldownRecordsByAuthID(records []cliproxyauth.CooldownStateRecord) map[string][]cliproxyauth.CooldownStateRecord {
	out := make(map[string][]cliproxyauth.CooldownStateRecord)
	for _, rec := range records {
		id := strings.TrimSpace(rec.AuthID)
		if id == "" {
			continue
		}
		out[id] = append(out[id], rec)
	}
	return out
}

// marshalCooldownEnvelope builds and marshals the JSON envelope for a group
// of records belonging to the same AuthID, sorted by Model for determinism.
func marshalCooldownEnvelope(authID, provider string, records []cliproxyauth.CooldownStateRecord) ([]byte, error) {
	sorted := append([]cliproxyauth.CooldownStateRecord(nil), records...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Model < sorted[j].Model
	})
	env := cooldownStateEnvelope{
		Version:   1,
		AuthID:    authID,
		Provider:  provider,
		UpdatedAt: time.Now().UTC(),
		Records:   sorted,
	}
	return json.Marshal(env)
}

func (c *PostgresCooldownStateStore) tableName() string {
	if c == nil || c.s == nil {
		return ""
	}
	name := c.s.cfg.CooldownTable
	if name == "" {
		name = defaultCooldownTable
	}
	return c.s.fullTableName(name)
}

// Load reads all cooldown rows and flattens their records into a single slice.
func (c *PostgresCooldownStateStore) Load(ctx context.Context) ([]cliproxyauth.CooldownStateRecord, error) {
	if c == nil || c.s == nil || c.s.db == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.s.mu.Lock()
	defer c.s.mu.Unlock()

	query := fmt.Sprintf(`SELECT id, content FROM %s`, c.tableName())
	rows, err := c.s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("postgres cooldown: list: %w", err)
	}
	defer rows.Close()

	out := make([]cliproxyauth.CooldownStateRecord, 0)
	for rows.Next() {
		var id string
		var content []byte
		if err = rows.Scan(&id, &content); err != nil {
			return nil, fmt.Errorf("postgres cooldown: scan: %w", err)
		}
		var env cooldownStateEnvelope
		if err = json.Unmarshal(content, &env); err != nil {
			return nil, fmt.Errorf("postgres cooldown: parse %s: %w", id, err)
		}
		out = append(out, env.Records...)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres cooldown: iterate: %w", err)
	}
	return out, nil
}

// Save persists a full snapshot of cooldown records, grouped by AuthID, and
// deletes any rows no longer present in the snapshot (full-wipe semantics).
// All upserts and stale deletes run inside a single DB transaction so one
// Save call is atomic for single-process consistency: a partial failure
// rolls back the whole batch instead of leaving a half-written snapshot.
// This does NOT provide multi-instance strong consistency; Mode A
// (last-write-wins across instances) remains the documented behavior.
func (c *PostgresCooldownStateStore) Save(ctx context.Context, records []cliproxyauth.CooldownStateRecord) error {
	if c == nil || c.s == nil || c.s.db == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	c.s.mu.Lock()
	defer c.s.mu.Unlock()

	groups := groupCooldownRecordsByAuthID(records)
	table := c.tableName()

	tx, err := c.s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres cooldown: begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	desired := make(map[string]struct{}, len(groups))
	for authID, group := range groups {
		provider := ""
		if len(group) > 0 {
			provider = strings.TrimSpace(group[0].Provider)
		}
		var payload []byte
		payload, err = marshalCooldownEnvelope(authID, provider, group)
		if err != nil {
			return fmt.Errorf("postgres cooldown: marshal %s: %w", authID, err)
		}
		// Mark as desired once the envelope is ready to write; if the
		// upsert below fails we return immediately and the whole
		// transaction rolls back, so the set never outlives a partial write.
		desired[authID] = struct{}{}
		query := fmt.Sprintf(`
			INSERT INTO %s (id, content, created_at, updated_at)
			VALUES ($1, $2, NOW(), NOW())
			ON CONFLICT (id)
			DO UPDATE SET content = EXCLUDED.content, updated_at = NOW()
		`, table)
		if _, err = tx.ExecContext(ctx, query, authID, json.RawMessage(payload)); err != nil {
			return fmt.Errorf("postgres cooldown: upsert %s: %w", authID, err)
		}
	}

	// Stale cleanup: remove rows not present in this snapshot, within the
	// same transaction as the upserts above.
	var rows *sql.Rows
	rows, err = tx.QueryContext(ctx, fmt.Sprintf(`SELECT id FROM %s`, table))
	if err != nil {
		return fmt.Errorf("postgres cooldown: list ids: %w", err)
	}

	stale := make([]string, 0)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("postgres cooldown: scan id: %w", err)
		}
		if _, ok := desired[id]; !ok {
			stale = append(stale, id)
		}
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("postgres cooldown: iterate ids: %w", err)
	}
	_ = rows.Close()

	for _, id := range stale {
		if err = ctx.Err(); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, table), id); err != nil {
			return fmt.Errorf("postgres cooldown: delete %s: %w", id, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("postgres cooldown: commit: %w", err)
	}
	return nil
}

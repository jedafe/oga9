// Ref: oga9-engineering/AGENTS.md Rule 11 + .oos conventions —
// platform:data_erasure is the ONLY hard-delete path. Two shapes:
// {user_id} erases one user's personal data; {} sweeps soft-deleted rows
// past platform.erasure.grace_days (system_settings, default 30).
package platform

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jedafe/oga9/internal/audit"
	"github.com/jedafe/oga9/internal/db"
)

// PurgeExpired hard-deletes rows whose deleted_at is older than the grace
// window. Member-facing content (documents, chat messages) and org/user
// shells purge here — Rule 11: no other path may hard-delete.
func (s *Service) PurgeExpired(ctx context.Context, graceDays int) (int64, error) {
	if graceDays <= 0 {
		graceDays = 30
	}
	var total int64
	// collect blob keys before the row deletes so files don't orphan —
	// erasure without the blob is not erasure.
	if s.store != nil {
		rows, rerr := s.pool.Query(ctx, `SELECT storage_key FROM documents
			WHERE deleted_at IS NOT NULL AND deleted_at < now() - make_interval(days => $1::int)`, graceDays)
		if rerr == nil {
			var keys []string
			for rows.Next() {
				var k string
				if rows.Scan(&k) == nil {
					keys = append(keys, k)
				}
			}
			rows.Close()
			defer func() {
				for _, k := range keys {
					if err := s.store.Remove(ctx, k); err != nil {
						slog.Warn("data_erasure: blob remove failed", "key", k, "err", err)
					}
				}
			}()
		}
	}
	for _, st := range []string{
		`DELETE FROM document_versions dv USING documents d
		 WHERE dv.document_id = d.id
		   AND d.deleted_at IS NOT NULL AND d.deleted_at < now() - make_interval(days => $1::int)`,
		`DELETE FROM documents WHERE deleted_at IS NOT NULL
		   AND deleted_at < now() - make_interval(days => $1::int)`,
		`DELETE FROM chat_messages WHERE deleted_at IS NOT NULL
		   AND deleted_at < now() - make_interval(days => $1::int)`,
		`DELETE FROM user_sessions us USING users u
		 WHERE us.user_id = u.id AND u.deleted_at IS NOT NULL
		   AND u.deleted_at < now() - make_interval(days => $1::int)`,
		`DELETE FROM oauth_providers op USING users u
		 WHERE op.user_id = u.id AND u.deleted_at IS NOT NULL
		   AND u.deleted_at < now() - make_interval(days => $1::int)`,
	} {
		tag, err := s.pool.Exec(ctx, st, graceDays)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
	}
	if total > 0 {
		slog.Info("data_erasure: purged soft-deleted rows", "rows", total, "grace_days", graceDays)
	}
	return total, nil
}

// EraseUser removes a user's personal data: PII anonymized on the users
// row, sessions + OAuth links hard-deleted. Membership/finance/audit rows
// stay — institutional records are term-scoped history (Rule 21); the
// erasure removes the person, not the org's books.
func (s *Service) EraseUser(ctx context.Context, userID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	uid := pgtype.UUID{Bytes: userID, Valid: true}
	// audit FIRST — a failed audit write aborts the erasure (Rule 10 spirit).
	if err := audit.Log(ctx, tx, audit.Entry{
		Action:     "data_erasure.user",
		TargetType: "user", TargetID: &userID,
		Context: map[string]any{"scope": "user_pii", "by": "platform:data_erasure"},
	}); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx,
		`UPDATE users SET email = 'erased-' || id::text || '@erased.local',
		   display_name = 'erased', password_hash = '', mfa_secret = NULL,
		   mfa_recovery_codes = NULL, status = 'erased', deleted_at = COALESCE(deleted_at, now())
		 WHERE id = $1 AND status <> 'erased'`, uid)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // already erased or unknown — idempotent (Rule 27)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id = $1`, uid); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM oauth_providers WHERE user_id = $1`, uid); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ErasureGraceDays reads platform.erasure.grace_days (system_settings).
func (s *Service) ErasureGraceDays(ctx context.Context) int {
	raw, err := db.New(s.pool).GetSystemSetting(ctx, "platform.erasure.grace_days")
	if err != nil {
		return 30
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil || n <= 0 {
		return 30
	}
	return n
}

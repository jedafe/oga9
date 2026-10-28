// Ref: oga9-engineering/sections/09-deployment-editions.md + ADR-026 —
// oga9-admin is the self-hosted operations CLI shipped in the §09 bundle.
//
// Subcommands:
//
//	import — load a cloud tenant_export_v1 bundle into this install
//
// import verifies the platform signature + per-member checksums, then
// bulk-loads every table under session_replication_role=replica (FK
// triggers off — the bundle carries no topological order). Re-runs are
// safe: every insert is ON CONFLICT DO NOTHING.
package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/jedafe/oga9/internal/tenantexport"
)

var ident = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "import":
		if err := runImport(os.Args[2:]); err != nil {
			log.Fatalf("import: %v", err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `oga9-admin — self-hosted operations

  oga9-admin import --bundle <file.oga9exp> [flags]

Flags:
  --dsn          database URL       (env OGA9_DATABASE_URL)
  --pubkey       hex ed25519 public key for signature verify
                 (env OGA9_LICENSE_PUBLIC_KEY)
  --passphrase   bundle passphrase for encrypted exports
                 (env OGA9_EXPORT_PASSPHRASE)
  --uploads      blob directory     (env OGA9_UPLOADS_DIR, default ./data/uploads)
  --allow-unsigned   import without verifying the platform signature`)
}

func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	bundlePath := fs.String("bundle", "", "tenant_export_v1 bundle path")
	dsn := fs.String("dsn", os.Getenv("OGA9_DATABASE_URL"), "database URL")
	pubHex := fs.String("pubkey", os.Getenv("OGA9_LICENSE_PUBLIC_KEY"), "hex ed25519 public key")
	passphrase := fs.String("passphrase", os.Getenv("OGA9_EXPORT_PASSPHRASE"), "bundle passphrase")
	uploads := fs.String("uploads", envOr("OGA9_UPLOADS_DIR", "./data/uploads"), "blob directory")
	allowUnsigned := fs.Bool("allow-unsigned", false, "skip signature verification")
	_ = fs.Parse(args)
	if *bundlePath == "" {
		return errors.New("--bundle is required")
	}
	if *dsn == "" {
		return errors.New("--dsn or OGA9_DATABASE_URL required")
	}

	f, err := os.Open(*bundlePath)
	if err != nil {
		return err
	}
	defer f.Close()
	a, err := tenantexport.Open(f, *passphrase)
	if err != nil {
		return err
	}
	defer a.Close()
	m := a.Manifest()

	verified := false
	if *pubHex != "" {
		pub, err := hex.DecodeString(*pubHex)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return errors.New("bad --pubkey (hex ed25519 public key expected)")
		}
		if err := a.Verify(ed25519.PublicKey(pub)); err != nil {
			return err
		}
		verified = true
	} else {
		if !*allowUnsigned {
			return errors.New("no --pubkey given — pass OGA9_LICENSE_PUBLIC_KEY or --allow-unsigned")
		}
		if !m.Unsigned {
			log.Printf("warning: signed bundle imported WITHOUT signature check (--allow-unsigned)")
		}
		if err := a.Verify(nil); err != nil {
			return err // checksums still enforced
		}
	}
	log.Printf("bundle %s tenant=%s version=%d signed=%v verified=%v credentials=%v",
		m.Format, m.TenantID, m.BundleVersion, !m.Unsigned, verified, m.IncludeCredentials)
	if len(m.DroppedExternal) > 0 {
		for _, d := range m.DroppedExternal {
			var rows []json.RawMessage
			_ = json.Unmarshal(d.Rows, &rows)
			log.Printf("note: %d %s edges to orgs outside the tenant were not imported", len(rows), d.Table)
		}
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	// Self-hosted is single-tenant — a different tenant already on this
	// install means this bundle does not belong here.
	var existingID string
	err = conn.QueryRow(ctx, "SELECT id::text FROM tenants LIMIT 1").Scan(&existingID)
	if err != nil && err != pgx.ErrNoRows {
		return err
	}
	if existingID != "" && existingID != m.TenantID {
		return fmt.Errorf("this install already hosts tenant %s — self-hosted is single-tenant (refusing to import %s)", existingID, m.TenantID)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// FK triggers off — bundle tables carry no topological order.
	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = 'replica'"); err != nil {
		return fmt.Errorf("disabling FK checks needs a superuser connection: %w", err)
	}
	existing, colSets, err := listTables(ctx, tx)
	if err != nil {
		return err
	}
	inserted := map[string]int64{}
	err = a.Walk(func(path string, r io.Reader, _ int64) error {
		if !strings.HasPrefix(path, "data/") || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		table := strings.TrimSuffix(strings.TrimPrefix(path, "data/"), ".jsonl")
		if !ident.MatchString(table) {
			return fmt.Errorf("unsafe table name in bundle: %q", table)
		}
		if !existing[table] {
			log.Printf("skip %s — table not present in this schema version", table)
			return nil
		}
		n, lines, err := insertJSONL(ctx, tx, table, r, colSets[table])
		inserted[table] += n
		if int64(lines) > n {
			log.Printf("  note: %s — %d of %d rows conflicted with existing rows and were skipped", table, int64(lines)-n, lines)
		}
		return err
	})
	if err != nil {
		return err
	}
	// the imported tenant now lives on a self-hosted install.
	if _, err := tx.Exec(ctx,
		"UPDATE tenants SET edition='self_hosted' WHERE id = $1::uuid", m.TenantID); err != nil {
		return err
	}
	// imported outbox rows keep their history but must never re-fire —
	// a published_at IS NULL row would be claimed by the dispatcher.
	if _, err := tx.Exec(ctx,
		"UPDATE domain_events SET published_at = now() WHERE published_at IS NULL"); err != nil {
		return err
	}
	if m.IncludeCredentials {
		var mfa int
		_ = tx.QueryRow(ctx,
			"SELECT count(*) FROM users WHERE mfa_secret IS NOT NULL").Scan(&mfa)
		if mfa > 0 {
			log.Printf("warning: %d users carry MFA secrets — they only decrypt under the source install's OGA9_DATA_ENCRYPTION_KEY; use the same key here or affected users must reset MFA", mfa)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// blobs land only after the data tx commits — a failed import must
	// not half-populate the uploads dir.
	var nFiles int
	if err := os.MkdirAll(*uploads, 0o755); err != nil {
		return err
	}
	err = a.Walk(func(path string, r io.Reader, _ int64) error {
		if !strings.HasPrefix(path, "files/") {
			return nil
		}
		key := filepath.Clean("/" + strings.ReplaceAll(strings.TrimPrefix(path, "files/"), "..", ""))
		dst, err := os.Create(filepath.Join(*uploads, key))
		if err != nil {
			return err
		}
		defer dst.Close()
		if _, err := io.Copy(dst, r); err != nil {
			return err
		}
		nFiles++
		return nil
	})
	if err != nil {
		return err
	}
	var total int64
	for t, n := range inserted {
		if n > 0 {
			log.Printf("  %-40s %d", t, n)
		}
		total += n
	}
	log.Printf("imported %d rows, %d blobs — attach the deployment's license file (OGA9_LICENSE_FILE) if not yet present", total, nFiles)
	return nil
}

// listTables returns every BASE TABLE plus its column set — the column
// map keeps bundle keys honest (unknown keys are skipped, and absent
// columns fall back to their schema DEFAULT rather than explicit NULL).
func listTables(ctx context.Context, tx pgx.Tx) (map[string]bool, map[string]map[string]bool, error) {
	rows, err := tx.Query(ctx,
		`SELECT c.table_name::text, c.column_name::text FROM information_schema.columns c
		 JOIN information_schema.tables t ON t.table_name = c.table_name AND t.table_schema = c.table_schema
		 WHERE c.table_schema='public' AND t.table_type='BASE TABLE'`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	tables := map[string]bool{}
	cols := map[string]map[string]bool{}
	for rows.Next() {
		var t, c string
		if err := rows.Scan(&t, &c); err != nil {
			return nil, nil, err
		}
		tables[t] = true
		if cols[t] == nil {
			cols[t] = map[string]bool{}
		}
		cols[t][c] = true
	}
	return tables, cols, rows.Err()
}

// insertJSONL batches lines into INSERTs projecting only the keys the
// rows actually carry — a credential-free projection omits columns like
// password_hash on purpose, and explicit NULLs would defeat NOT NULL
// defaults (mfa_recovery_codes …) or violate constraints outright.
// Rows stream line-by-line — a table file is never held whole in memory.
func insertJSONL(ctx context.Context, tx pgx.Tx, table string, data io.Reader, cols map[string]bool) (int64, int, error) {
	var batch []json.RawMessage
	var total int64
	var lines int
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		// union of keys present in this batch, intersected with real cols.
		keySet := map[string]bool{}
		for _, raw := range batch {
			var row map[string]json.RawMessage
			if json.Unmarshal(raw, &row) != nil {
				continue
			}
			for k := range row {
				if cols[k] && ident.MatchString(k) {
					keySet[k] = true
				}
			}
		}
		var keys []string
		for k := range keySet {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) == 0 {
			batch = batch[:0]
			return nil
		}
		quoted := make([]string, len(keys))
		for i, k := range keys {
			quoted[i] = `"` + k + `"`
		}
		list := strings.Join(quoted, ",")
		arr, _ := json.Marshal(batch)
		tag, err := tx.Exec(ctx, fmt.Sprintf(
			`INSERT INTO "%s" (%s) SELECT %s FROM jsonb_populate_recordset(NULL::"%s", $1::jsonb) r ON CONFLICT DO NOTHING`,
			table, list, list, table), arr)
		if err != nil {
			return fmt.Errorf("insert %s: %w", table, err)
		}
		total += tag.RowsAffected()
		batch = batch[:0]
		return nil
	}
	sc := bufio.NewScanner(data)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		lines++
		batch = append(batch, json.RawMessage(line))
		if len(batch) >= 500 {
			if err := flush(); err != nil {
				return total, lines, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return total, lines, err
	}
	return total, lines, flush()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

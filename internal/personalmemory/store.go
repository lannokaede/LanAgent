// Package personalmemory stores explicit personal facts. SQLite is authoritative;
// vector search results are candidates and are always checked against current rows.
package personalmemory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

var ErrConflict = errors.New("memory version conflict; list current memories and retry")
var ErrOperation = errors.New("memory operation ID was already used with different input")
var keyPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

type Record struct {
	ID        string `json:"id"`
	Scope     string `json:"scope"`
	Key       string `json:"key"`
	Content   string `json:"content"`
	Source    string `json:"source"`
	Version   int64  `json:"version"`
	Deleted   bool   `json:"deleted"`
	VectorID  string `json:"vector_id"`
	UpdatedAt int64  `json:"updated_at"`
}
type Store struct {
	db       *sql.DB
	identity string
}

func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

const schema = `
CREATE TABLE IF NOT EXISTS pm_meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS pm_facts(
 id TEXT PRIMARY KEY,scope TEXT NOT NULL,key TEXT NOT NULL,content TEXT NOT NULL,
 source TEXT NOT NULL,version INTEGER NOT NULL CHECK(version>0),deleted INTEGER NOT NULL,
 vector_id TEXT NOT NULL UNIQUE,updated_at INTEGER NOT NULL,UNIQUE(scope,key));
CREATE TABLE IF NOT EXISTS pm_operations(id TEXT PRIMARY KEY,digest TEXT NOT NULL,result BLOB);
CREATE TABLE IF NOT EXISTS pm_outbox(
 id INTEGER PRIMARY KEY AUTOINCREMENT,memory_id TEXT NOT NULL,version INTEGER NOT NULL,
 payload BLOB NOT NULL,status TEXT NOT NULL DEFAULT 'pending',attempts INTEGER NOT NULL DEFAULT 0,
 available_at INTEGER NOT NULL DEFAULT 0,lease_until INTEGER NOT NULL DEFAULT 0,owner TEXT NOT NULL DEFAULT '',
 UNIQUE(memory_id,version));
CREATE INDEX IF NOT EXISTS pm_ready ON pm_outbox(status,available_at);
`

func Open(ctx context.Context, path string) (*Store, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return nil, err
	}
	// Create privately before SQLite opens it; WAL/SHM inherit database permissions.
	f, err := os.OpenFile(absolute, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(e error) (*Store, error) { db.Close(); return nil, e }
	if _, err = db.ExecContext(ctx, schema); err != nil {
		return fail(err)
	}
	if _, err = db.ExecContext(ctx, "INSERT OR IGNORE INTO pm_meta(key,value) VALUES('identity',?)", rand.Text()); err != nil {
		return fail(err)
	}
	s := &Store{db: db}
	if err = db.QueryRowContext(ctx, "SELECT value FROM pm_meta WHERE key='identity'").Scan(&s.identity); err != nil {
		return fail(err)
	}
	var mode string
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fail(err)
	}
	if mode != "wal" {
		return fail(errors.New("personal memory requires WAL mode"))
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }

// Scope identifiers include the durable database identity, preventing a new DB
// from accepting abandoned vectors belonging to a previous local profile.
func (s *Store) UserScope() string { return "user_" + hash(s.identity)[:40] }
func (s *Store) ProjectScope(workspace string) string {
	return "project_" + hash(s.identity + "\x00" + workspace)[:40]
}

const columns = "id,scope,key,content,source,version,deleted,vector_id,updated_at"

type scanner interface{ Scan(...any) error }

func scan(row scanner) (r Record, err error) {
	err = row.Scan(&r.ID, &r.Scope, &r.Key, &r.Content, &r.Source, &r.Version, &r.Deleted, &r.VectorID, &r.UpdatedAt)
	return
}
func (s *Store) Get(ctx context.Context, scope, key string) (Record, error) {
	return scan(s.db.QueryRowContext(ctx, "SELECT "+columns+" FROM pm_facts WHERE scope=? AND key=?", scope, key))
}
func (s *Store) List(ctx context.Context, scope string, includeDeleted bool) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+columns+" FROM pm_facts WHERE scope=? AND (deleted=0 OR ?) ORDER BY updated_at DESC,id LIMIT 200", scope, includeDeleted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []Record{}
	for rows.Next() {
		r, e := scan(rows)
		if e != nil {
			return nil, e
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// Mutate atomically saves a fact, its immutable synchronization task and receipt.
// expected=0 creates; every correction/delete must name the current version.
// Reusing op with identical input returns its original receipt, even after edits.
func (s *Store) Mutate(ctx context.Context, op, scope, key, content, source string, expected int64, deleted bool) (Record, error) {
	if !keyPattern.MatchString(key) || len(op) == 0 || len(op) > 128 || len(scope) > 64 || (!strings.HasPrefix(scope, "user_") && !strings.HasPrefix(scope, "project_")) || expected < 0 || len(content) > 4096 || !utf8.ValidString(content) || strings.ContainsRune(content, 0) || len(source) > 512 || (!deleted && strings.TrimSpace(content) == "") {
		return Record{}, errors.New("invalid memory mutation")
	}
	if deleted {
		content = ""
	}
	raw, _ := json.Marshal([]any{scope, key, content, source, expected, deleted})
	digest := hash(string(raw))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback()
	// The first statement takes the write lock before reading a version, avoiding
	// deferred read-to-write upgrades across independent CLI processes.
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO pm_operations(id,digest) VALUES(?,?)", op, digest); err != nil {
		return Record{}, err
	}
	var previous string
	var result []byte
	if err = tx.QueryRowContext(ctx, "SELECT digest,result FROM pm_operations WHERE id=?", op).Scan(&previous, &result); err != nil {
		return Record{}, err
	}
	if previous != digest {
		return Record{}, ErrOperation
	}
	if len(result) > 0 {
		var r Record
		if err = json.Unmarshal(result, &r); err != nil {
			return Record{}, err
		}
		return r, tx.Commit()
	}
	old, err := scan(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM pm_facts WHERE scope=? AND key=?", scope, key))
	if errors.Is(err, sql.ErrNoRows) {
		if expected != 0 || deleted {
			return Record{}, ErrConflict
		}
	} else if err != nil {
		return Record{}, err
	} else if old.Version != expected {
		return Record{}, ErrConflict
	}
	r := Record{ID: hash(scope + "\x00" + key), Scope: scope, Key: key, Content: content, Source: source, Version: expected + 1, Deleted: deleted, UpdatedAt: time.Now().UnixMilli()}
	r.VectorID = hash(fmt.Sprintf("%s/%d", r.ID, r.Version))
	if _, err = tx.ExecContext(ctx, `INSERT INTO pm_facts(`+columns+`) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET content=excluded.content,source=excluded.source,version=excluded.version,deleted=excluded.deleted,vector_id=excluded.vector_id,updated_at=excluded.updated_at`, r.ID, r.Scope, r.Key, r.Content, r.Source, r.Version, r.Deleted, r.VectorID, r.UpdatedAt); err != nil {
		return Record{}, err
	}
	result, _ = json.Marshal(r)
	if _, err = tx.ExecContext(ctx, "INSERT INTO pm_outbox(memory_id,version,payload) VALUES(?,?,?)", r.ID, r.Version, result); err != nil {
		return Record{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE pm_operations SET result=? WHERE id=?", result, op); err != nil {
		return Record{}, err
	}
	return r, tx.Commit()
}

// Resolve never trusts vector text: only the current, live SQLite version is read.
func (s *Store) Resolve(ctx context.Context, vectorID string, scopes []string) (Record, error) {
	if len(scopes) != 2 {
		return Record{}, errors.New("expected user and project scopes")
	}
	return scan(s.db.QueryRowContext(ctx, "SELECT "+columns+" FROM pm_facts WHERE vector_id=? AND deleted=0 AND scope IN (?,?)", vectorID, scopes[0], scopes[1]))
}
func (s *Store) BindIndex(ctx context.Context, identity string) error {
	if _, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO pm_meta(key,value) VALUES('index',?)", identity); err != nil {
		return err
	}
	var current string
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM pm_meta WHERE key='index'").Scan(&current); err != nil {
		return err
	}
	if current != identity {
		return errors.New("memory index identity changed; use the original collection/model or a separate memory database")
	}
	return nil
}

type Job struct {
	ID       int64
	Record   Record
	Owner    string
	Attempts int
}

func (s *Store) claim(ctx context.Context) (*Job, error) {
	now := time.Now().UnixMilli()
	j := &Job{Owner: rand.Text()}
	var payload []byte
	err := s.db.QueryRowContext(ctx, `UPDATE pm_outbox SET status='running',owner=?,attempts=attempts+1,lease_until=?
 WHERE id=(SELECT id FROM pm_outbox WHERE (status='pending' AND available_at<=?) OR (status='running' AND lease_until<=?) ORDER BY id LIMIT 1)
 RETURNING id,payload,attempts`, j.Owner, now+120000, now, now).Scan(&j.ID, &payload, &j.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(payload, &j.Record); err != nil {
		return nil, err
	}
	return j, nil
}
func (s *Store) finish(ctx context.Context, j *Job, callErr error) error {
	status := "done"
	if callErr != nil {
		status = "pending"
		if j.Attempts >= 5 {
			status = "failed"
		}
	}
	_, err := s.db.ExecContext(ctx, "UPDATE pm_outbox SET status=?,available_at=?,lease_until=0 WHERE id=? AND owner=? AND status='running'", status, time.Now().Add(time.Duration(1<<min(j.Attempts, 8))*time.Second).UnixMilli(), j.ID, j.Owner)
	return err
}
func (s *Store) Status(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT status,count(*) FROM pm_outbox GROUP BY status")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err = rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}
func (s *Store) Retry(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "UPDATE pm_outbox SET status='pending',attempts=0,available_at=0 WHERE status='failed'")
	return err
}

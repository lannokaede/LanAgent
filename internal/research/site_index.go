package research

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// SiteIndex is independent of both personal memory and the vector knowledge base.
type SiteIndex struct{ db *sql.DB }

func OpenSiteIndex(dir string) (*SiteIndex, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	path, e := filepath.Abs(filepath.Join(dir, "web-index.db"))
	if e != nil {
		return nil, e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = f.Close(); e != nil {
		return nil, e
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS pages(id INTEGER PRIMARY KEY,url TEXT UNIQUE NOT NULL,title TEXT NOT NULL,body TEXT NOT NULL,hash TEXT NOT NULL,fetched_at TEXT NOT NULL);
 CREATE VIRTUAL TABLE IF NOT EXISTS words USING fts5(title,body,tokenize='unicode61');
 CREATE VIRTUAL TABLE IF NOT EXISTS grams USING fts5(title,body,tokenize='trigram');
 CREATE TABLE IF NOT EXISTS attempts(url TEXT PRIMARY KEY,attempted_at TEXT NOT NULL,error TEXT NOT NULL);`)
	if e != nil {
		db.Close()
		return nil, e
	}
	return &SiteIndex{db}, nil
}
func (i *SiteIndex) Close() error { return i.db.Close() }
func (i *SiteIndex) Put(ctx context.Context, p Page) error {
	if p.URL == "" || len(p.Text) == 0 || len(p.Text) > MaxBody || !utf8.ValidString(p.Text) || strings.ContainsRune(p.Text, 0) {
		return errors.New("invalid index page")
	}
	p.Title = preview(strings.TrimSpace(p.Title), 512)
	tx, e := i.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	h := hash([]byte(p.Title + "\n" + p.Text))
	at := now()
	var id int64
	var old string
	e = tx.QueryRowContext(ctx, "SELECT id,hash FROM pages WHERE url=?", p.URL).Scan(&id, &old)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	if e == sql.ErrNoRows {
		var count int
		if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM pages").Scan(&count); e != nil {
			return e
		}
		if count >= 4096 {
			return errors.New("web index capacity reached (4096 pages)")
		}
		r, e := tx.ExecContext(ctx, "INSERT INTO pages(url,title,body,hash,fetched_at) VALUES(?,?,?,?,?)", p.URL, p.Title, p.Text, h, at)
		if e != nil {
			return e
		}
		id, e = r.LastInsertId()
		if e != nil {
			return e
		}
	} else {
		if _, e = tx.ExecContext(ctx, "UPDATE pages SET title=?,body=?,hash=?,fetched_at=? WHERE id=?", p.Title, p.Text, h, at, id); e != nil {
			return e
		}
	}
	if old != h {
		for _, table := range []string{"words", "grams"} {
			if _, e = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE rowid=?", id); e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO "+table+"(rowid,title,body) VALUES(?,?,?)", id, p.Title, p.Text); e != nil {
				return e
			}
		}
	}
	if _, e = tx.ExecContext(ctx, "INSERT OR REPLACE INTO attempts VALUES(?,?,?)", p.URL, at, ""); e != nil {
		return e
	}
	return tx.Commit()
}
func (i *SiteIndex) Failure(ctx context.Context, u string, cause error) error {
	_, e := i.db.ExecContext(ctx, "INSERT OR REPLACE INTO attempts VALUES(?,?,?)", u, now(), preview(cause.Error(), 1024))
	return e
}
func (i *SiteIndex) Search(ctx context.Context, q string, n int) ([]Page, error) {
	if strings.TrimSpace(q) == "" || utf8.RuneCountInString(q) > 1024 || n < 1 || n > 5 {
		return nil, errors.New("query requires 1..1024 characters, limit 1..5")
	}
	var count int
	if e := i.db.QueryRowContext(ctx, "SELECT count(*) FROM pages").Scan(&count); e != nil {
		return nil, e
	}
	if count == 0 {
		return nil, errors.New("official web index is empty; run research-mcp index refresh -data-dir <research-directory> first")
	}
	terms := strings.FieldsFunc(q, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(terms) > 16 {
		terms = terms[:16]
	}
	if len(terms) == 0 {
		return []Page{}, nil
	}
	var words, grams, short []string
	for _, t := range terms {
		quoted := `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
		words = append(words, quoted)
		if utf8.RuneCountInString(t) >= 3 {
			grams = append(grams, quoted)
		} else {
			short = append(short, strings.ToLower(t))
		}
	}
	clauses := []string{"SELECT rowid AS id,bm25(words,5.0,1.0) AS score FROM words WHERE words MATCH ?"}
	args := []any{strings.Join(words, " OR ")}
	if len(grams) > 0 {
		clauses = append(clauses, "SELECT rowid AS id,bm25(grams,5.0,1.0) AS score FROM grams WHERE grams MATCH ?")
		args = append(args, strings.Join(grams, " OR "))
	}
	// The substring fallback only inspects bounded excerpts of at most 1000 pages.
	for _, t := range short {
		clauses = append(clauses, "SELECT id,CASE WHEN instr(lower(title),?)>0 THEN -0.000002 ELSE -0.000001 END AS score FROM (SELECT id,title,substr(body,1,16384) AS body FROM pages ORDER BY fetched_at DESC,id LIMIT 1000) WHERE instr(lower(title),?)>0 OR instr(lower(body),?)>0")
		args = append(args, t, t, t)
	}
	query := `WITH hits AS (` + strings.Join(clauses, " UNION ALL ") + `) SELECT p.url,p.title,substr(p.body,1,16384),p.fetched_at FROM pages p JOIN (SELECT id,min(score) AS score FROM hits GROUP BY id) h ON p.id=h.id ORDER BY h.score,p.url LIMIT ?`
	args = append(args, n)
	rows, e := i.db.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, fmt.Errorf("search site index: %w", e)
	}
	defer rows.Close()
	out := []Page{}
	for rows.Next() {
		var p Page
		var body string
		if e = rows.Scan(&p.URL, &p.Title, &body, &p.FetchedAt); e != nil {
			return nil, e
		}
		p.Description = excerpt(body, terms)
		p.Origin = "local_site_index"
		out = append(out, p)
	}
	return out, rows.Err()
}
func excerpt(body string, terms []string) string {
	lower := strings.ToLower(body)
	start := 0
	for _, t := range terms {
		if pos := strings.Index(lower, strings.ToLower(t)); pos >= 0 {
			start = pos - 100
			if start < 0 {
				start = 0
			}
			break
		}
	}
	// Unicode case conversion can change byte lengths; clamp before slicing.
	if start >= len(body) {
		start = 0
	}
	for start > 0 && !utf8.RuneStart(body[start]) {
		start--
	}
	return preview(body[start:], 600)
}

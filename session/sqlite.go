package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	korai "github.com/korai-one/korai-sdk-go"

	_ "modernc.org/sqlite" // pure-Go SQLite driver registered as "sqlite"
)

// sqliteDriver is the database/sql driver name registered by modernc.org/sqlite.
const sqliteDriver = "sqlite"

// schema is the SQLite migration. One row per session holds the full serialized
// message list as a JSON blob (canonical korai.SessionMessage JSON, passed
// through the Codec). created/updated are stored as RFC 3339 nanosecond strings
// to round-trip time.Time exactly.
const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id       TEXT PRIMARY KEY,
	created  TEXT NOT NULL,
	updated  TEXT NOT NULL,
	cwd      TEXT NOT NULL,
	model    TEXT NOT NULL,
	tool     TEXT NOT NULL DEFAULT '',
	enc      TEXT NOT NULL,
	messages BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_cwd_updated ON sessions(cwd, updated);
`

// timeFormat is the textual encoding used for created/updated columns. RFC 3339
// with nanoseconds sorts lexically in time order and round-trips time.Time.
const timeFormat = time.RFC3339Nano

// SQLiteStore persists sessions to a single SQLite database file using the
// pure-Go modernc.org/sqlite driver via database/sql. It implements Store. Each
// session is one row whose messages column holds the serialized message list,
// passed through a Codec (the at-rest-encryption seam, plaintext by default).
type SQLiteStore struct {
	db    *sql.DB
	codec Codec
}

// NewSQLiteStore opens (creating if absent) the database at path and runs the
// schema migration. The parent directory is created 0700 and the database file
// is kept private to the user. The returned store uses the plaintext codec; call
// WithCodec to change it.
func NewSQLiteStore(ctx context.Context, path string) (*SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("creating session dir: %w", err)
	}
	db, err := sql.Open(sqliteDriver, path)
	if err != nil {
		return nil, fmt.Errorf("opening session db %s: %w", path, err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		if cerr := os.Chmod(path, filePerm); cerr != nil {
			_ = db.Close()
			return nil, fmt.Errorf("securing session db %s: %w", path, cerr)
		}
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating session db %s: %w", path, err)
	}
	return &SQLiteStore{db: db, codec: PlainCodec{}}, nil
}

// WithCodec sets the codec used to encode the messages blob (the seam for
// at-rest encryption) and returns the store for chaining. The codec's Name is
// recorded per row so Load can select the matching codec.
func (s *SQLiteStore) WithCodec(c Codec) *SQLiteStore {
	if c != nil {
		s.codec = c
	}
	return s
}

// Close releases the underlying database handle.
func (s *SQLiteStore) Close() error { return s.db.Close() }

// codecFor returns the codec named in a row. Mirrors FileStore.codecFor.
func (s *SQLiteStore) codecFor(name string) (Codec, error) {
	if name == "" || name == (PlainCodec{}).Name() {
		return PlainCodec{}, nil
	}
	if s.codec != nil && s.codec.Name() == name {
		return s.codec, nil
	}
	return nil, fmt.Errorf("no codec %q to decode session", name)
}

// encodeMessages serializes msgs to canonical JSON and through the codec.
func (s *SQLiteStore) encodeMessages(msgs []korai.SessionMessage) ([]byte, error) {
	data, err := json.Marshal(msgs)
	if err != nil {
		return nil, fmt.Errorf("encoding session messages: %w", err)
	}
	enc, err := s.codec.Encode(data)
	if err != nil {
		return nil, fmt.Errorf("encoding session messages: %w", err)
	}
	return enc, nil
}

// decodeMessages reverses encodeMessages using the codec named in the row.
func (s *SQLiteStore) decodeMessages(enc string, stored []byte) ([]korai.SessionMessage, error) {
	codec, err := s.codecFor(enc)
	if err != nil {
		return nil, err
	}
	plain, err := codec.Decode(stored)
	if err != nil {
		return nil, fmt.Errorf("decoding session messages: %w", err)
	}
	var msgs []korai.SessionMessage
	if err := json.Unmarshal(plain, &msgs); err != nil {
		return nil, fmt.Errorf("decoding session messages: %w", err)
	}
	return msgs, nil
}

// Save upserts the whole record. The messages blob and metadata are replaced on
// conflict, so it both creates and updates. Updated is taken from s.Updated.
func (s *SQLiteStore) Save(sess korai.Session) error {
	blob, err := s.encodeMessages(sess.Messages)
	if err != nil {
		return err
	}
	const q = `
INSERT INTO sessions (id, created, updated, cwd, model, tool, enc, messages)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	updated  = excluded.updated,
	cwd      = excluded.cwd,
	model    = excluded.model,
	tool     = excluded.tool,
	enc      = excluded.enc,
	messages = excluded.messages`
	_, err = s.db.ExecContext(context.Background(), q,
		sess.ID,
		sess.Created.Format(timeFormat),
		sess.Updated.Format(timeFormat),
		sess.CWD, sess.Model, sess.Tool, s.codec.Name(), blob,
	)
	if err != nil {
		return fmt.Errorf("saving session %s: %w", sess.ID, err)
	}
	return nil
}

// Load returns the session with the given id, or an error wrapping fs.ErrNotExist
// if no such session exists.
func (s *SQLiteStore) Load(id string) (korai.Session, error) {
	const q = `SELECT id, created, updated, cwd, model, tool, enc, messages FROM sessions WHERE id = ?`
	row := s.db.QueryRowContext(context.Background(), q, id)
	sess, err := scanSession(row, s)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return korai.Session{}, fmt.Errorf("reading session %s: %w", id, fs.ErrNotExist)
		}
		return korai.Session{}, fmt.Errorf("reading session %s: %w", id, err)
	}
	return sess, nil
}

// List returns all saved sessions, most recently updated first.
func (s *SQLiteStore) List() ([]korai.Session, error) {
	const q = `SELECT id, created, updated, cwd, model, tool, enc, messages FROM sessions ORDER BY updated DESC`
	rows, err := s.db.QueryContext(context.Background(), q)
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sessions []korai.Session
	for rows.Next() {
		sess, err := scanSession(rows, s)
		if err != nil {
			continue // skip unreadable rows rather than failing the list
		}
		sessions = append(sessions, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	return sessions, nil
}

// Delete removes the session row with the given id. A missing row is not an
// error (delete is idempotent), so a sync tombstone applies cleanly even when
// the session is absent locally. It is not part of the Store interface; callers
// that need it (the sync client applying tombstones) type-assert for it.
func (s *SQLiteStore) Delete(id string) error {
	const q = `DELETE FROM sessions WHERE id = ?`
	if _, err := s.db.ExecContext(context.Background(), q, id); err != nil {
		return fmt.Errorf("deleting session %s: %w", id, err)
	}
	return nil
}

// Latest returns the most recently updated session for cwd, if any.
func (s *SQLiteStore) Latest(cwd string) (korai.Session, bool, error) {
	const q = `SELECT id, created, updated, cwd, model, tool, enc, messages FROM sessions WHERE cwd = ? ORDER BY updated DESC LIMIT 1`
	row := s.db.QueryRowContext(context.Background(), q, cwd)
	sess, err := scanSession(row, s)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return korai.Session{}, false, nil
		}
		return korai.Session{}, false, fmt.Errorf("finding latest session for %s: %w", cwd, err)
	}
	return sess, true, nil
}

// scanner abstracts *sql.Row and *sql.Rows so scanSession serves Load/List/Latest.
type scanner interface {
	Scan(dest ...any) error
}

// scanSession reads one row into a korai.Session, decoding the messages blob.
func scanSession(sc scanner, s *SQLiteStore) (korai.Session, error) {
	var (
		sess             korai.Session
		created, updated string
		enc              string
		blob             []byte
	)
	if err := sc.Scan(&sess.ID, &created, &updated, &sess.CWD, &sess.Model, &sess.Tool, &enc, &blob); err != nil {
		return korai.Session{}, err
	}
	c, err := time.Parse(timeFormat, created)
	if err != nil {
		return korai.Session{}, fmt.Errorf("parsing created time: %w", err)
	}
	u, err := time.Parse(timeFormat, updated)
	if err != nil {
		return korai.Session{}, fmt.Errorf("parsing updated time: %w", err)
	}
	sess.Created, sess.Updated = c, u
	msgs, err := s.decodeMessages(enc, blob)
	if err != nil {
		return korai.Session{}, err
	}
	sess.Messages = msgs
	return sess, nil
}

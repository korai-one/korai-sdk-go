package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	korai "github.com/korai-one/korai-sdk-go"
)

// fileExt is the on-disk extension for a session file.
const fileExt = ".jsonl"

// formatVersion is the on-disk JSONL schema version, recorded in the header.
const formatVersion = 1

// kindHeader tags the first (metadata) line of a session file. Message lines
// carry no "kind" field (a korai.SessionMessage marshals to {role,blocks}), so
// the reader distinguishes them by the presence of this tag.
const kindHeader = "header"

// Permissions: session files hold conversation content and are kept private to
// the user, matching the directory.
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// Store is the persistence boundary callers depend on. It captures the public
// surface common to every backend (the JSONL FileStore and the SQLiteStore), so
// call sites can be wired to either. Backend-specific knobs (such as
// FileStore.WithCodec) are not part of the interface. All stores operate on the
// rich canonical korai.Session.
type Store interface {
	// Save persists s, creating or updating its session.
	Save(s korai.Session) error
	// Load returns the session with the given id, or an error wrapping
	// fs.ErrNotExist if no such session exists.
	Load(id string) (korai.Session, error)
	// List returns all saved sessions, most recently updated first.
	List() ([]korai.Session, error)
	// Latest returns the most recently updated session for cwd, if any.
	Latest(cwd string) (korai.Session, bool, error)
}

// headerDTO is the first line of a session file: metadata written once when the
// file is created. It records the codec name (Enc) so Load can decode message
// lines, and is always stored in the clear.
type headerDTO struct {
	Kind    string    `json:"kind"` // always kindHeader
	Version int       `json:"version"`
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	CWD     string    `json:"cwd"`
	Model   string    `json:"model"`
	Tool    string    `json:"tool,omitempty"`
	Enc     string    `json:"enc"` // codec name; "none" = plaintext
}

// kindPeek reads just the discriminator field of a stored line.
type kindPeek struct {
	Kind string `json:"kind"`
}

// FileStore is a directory of session files (one JSONL file per session). It
// implements Store.
type FileStore struct {
	dir   string
	codec Codec
}

// NewFileStore returns a store rooted at dir, using the plaintext codec. Files
// are created lazily on Save.
func NewFileStore(dir string) *FileStore { return &FileStore{dir: dir, codec: PlainCodec{}} }

// WithCodec sets the codec used to encode message lines (the seam for at-rest
// encryption) and returns the store for chaining. The codec's Name is recorded
// in each file's header so Load can select the matching codec.
func (s *FileStore) WithCodec(c Codec) *FileStore {
	if c != nil {
		s.codec = c
	}
	return s
}

func (s *FileStore) path(id string) string { return filepath.Join(s.dir, id+fileExt) }

// codecFor returns the codec named in a file header. Plaintext files need no
// codec; otherwise the store's configured codec must match (you cannot decode
// what you lack the key for).
func (s *FileStore) codecFor(name string) (Codec, error) {
	if name == "" || name == (PlainCodec{}).Name() {
		return PlainCodec{}, nil
	}
	if s.codec != nil && s.codec.Name() == name {
		return s.codec, nil
	}
	return nil, fmt.Errorf("no codec %q to decode session", name)
}

// Save persists s. In the common case it appends only the messages not yet on
// disk; if the file is missing, or history has shrunk relative to disk (as after
// compaction replaces it), the whole file is rewritten from the header. Korai
// only ever extends or wholesale-replaces history, so a same-or-longer length is
// treated as an append.
func (s *FileStore) Save(sess korai.Session) error {
	if err := os.MkdirAll(s.dir, dirPerm); err != nil {
		return fmt.Errorf("creating session dir: %w", err)
	}
	path := s.path(sess.ID)
	onDisk, err := s.countMessages(path)
	if err != nil {
		return err
	}
	if onDisk < 0 || onDisk > len(sess.Messages) {
		return s.rewrite(path, sess)
	}
	return s.appendMessages(path, sess.Messages[onDisk:])
}

// countMessages returns the number of message entries already in the file (total
// lines minus the header line), or -1 if the file does not exist. It counts
// newline bytes so arbitrarily long lines are handled.
func (s *FileStore) countMessages(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return -1, nil
		}
		return 0, fmt.Errorf("opening session %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	lines := 0
	buf := make([]byte, 32*1024)
	for {
		n, rerr := f.Read(buf)
		lines += bytes.Count(buf[:n], []byte{'\n'})
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return 0, fmt.Errorf("scanning session %s: %w", path, rerr)
		}
	}
	if lines == 0 {
		return 0, nil
	}
	return lines - 1, nil // exclude the header line
}

// rewrite truncates the file and writes the header followed by every message.
func (s *FileStore) rewrite(path string, sess korai.Session) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, filePerm)
	if err != nil {
		return fmt.Errorf("creating session %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("closing session %s: %w", path, cerr)
		}
	}()
	if cerr := f.Chmod(filePerm); cerr != nil {
		return fmt.Errorf("securing session %s: %w", path, cerr)
	}

	w := bufio.NewWriter(f)
	if err := s.writeHeader(w, sess); err != nil {
		return err
	}
	for _, m := range sess.Messages {
		if err := s.writeMessage(w, m); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing session %s: %w", path, err)
	}
	return nil
}

// appendMessages appends msgs to an existing session file. A no-op for none.
func (s *FileStore) appendMessages(path string, msgs []korai.SessionMessage) (err error) {
	if len(msgs) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, filePerm)
	if err != nil {
		return fmt.Errorf("opening session %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("closing session %s: %w", path, cerr)
		}
	}()

	w := bufio.NewWriter(f)
	for _, m := range msgs {
		if err := s.writeMessage(w, m); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("appending session %s: %w", path, err)
	}
	return nil
}

// writeHeader writes the plaintext header line.
func (s *FileStore) writeHeader(w *bufio.Writer, sess korai.Session) error {
	h := headerDTO{
		Kind: kindHeader, Version: formatVersion,
		ID: sess.ID, Created: sess.Created, CWD: sess.CWD,
		Model: sess.Model, Tool: sess.Tool, Enc: s.codec.Name(),
	}
	data, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("encoding session header: %w", err)
	}
	return writeLine(w, data)
}

// writeMessage encodes m through the codec and writes it as one line.
func (s *FileStore) writeMessage(w *bufio.Writer, m korai.SessionMessage) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding session message: %w", err)
	}
	enc, err := s.codec.Encode(data)
	if err != nil {
		return fmt.Errorf("encoding session message: %w", err)
	}
	return writeLine(w, enc)
}

func writeLine(w *bufio.Writer, data []byte) error {
	if _, err := w.Write(data); err != nil {
		return err
	}
	return w.WriteByte('\n')
}

// Load reads the session with the given id. Updated is set from the file's
// modification time.
func (s *FileStore) Load(id string) (korai.Session, error) {
	path := s.path(id)
	f, err := os.Open(path)
	if err != nil {
		return korai.Session{}, fmt.Errorf("reading session %s: %w", id, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return korai.Session{}, fmt.Errorf("stat session %s: %w", id, err)
	}

	r := bufio.NewReader(f)
	header, err := readHeader(r)
	if err != nil {
		return korai.Session{}, fmt.Errorf("session %s: %w", id, err)
	}
	codec, err := s.codecFor(header.Enc)
	if err != nil {
		return korai.Session{}, fmt.Errorf("session %s: %w", id, err)
	}

	sess := korai.Session{
		ID: header.ID, Created: header.Created, Updated: info.ModTime(),
		CWD: header.CWD, Model: header.Model, Tool: header.Tool,
	}
	for {
		line, rerr := r.ReadBytes('\n')
		line = bytes.TrimRight(line, "\n")
		if len(line) > 0 {
			m, derr := decodeMessage(codec, line)
			if derr != nil {
				return korai.Session{}, fmt.Errorf("session %s: %w", id, derr)
			}
			sess.Messages = append(sess.Messages, m)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return korai.Session{}, fmt.Errorf("reading session %s: %w", id, rerr)
		}
	}
	return sess, nil
}

// readHeader reads and validates the first line as a session header.
func readHeader(r *bufio.Reader) (headerDTO, error) {
	line, err := r.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return headerDTO{}, fmt.Errorf("reading header: %w", err)
	}
	line = bytes.TrimRight(line, "\n")
	if len(line) == 0 {
		return headerDTO{}, errors.New("empty session file")
	}
	var h headerDTO
	if err := json.Unmarshal(line, &h); err != nil {
		return headerDTO{}, fmt.Errorf("decoding header: %w", err)
	}
	if h.Kind != kindHeader {
		return headerDTO{}, fmt.Errorf("first line is not a header (kind %q)", h.Kind)
	}
	return h, nil
}

// decodeMessage decodes one stored message line through the codec.
func decodeMessage(codec Codec, line []byte) (korai.SessionMessage, error) {
	plain, err := codec.Decode(line)
	if err != nil {
		return korai.SessionMessage{}, fmt.Errorf("decoding message: %w", err)
	}
	var peek kindPeek
	if err := json.Unmarshal(plain, &peek); err != nil {
		return korai.SessionMessage{}, fmt.Errorf("decoding message: %w", err)
	}
	if peek.Kind == kindHeader {
		return korai.SessionMessage{}, fmt.Errorf("unexpected header line among messages")
	}
	var m korai.SessionMessage
	if err := json.Unmarshal(plain, &m); err != nil {
		return korai.SessionMessage{}, fmt.Errorf("decoding message: %w", err)
	}
	return m, nil
}

// List returns all saved sessions, most recently modified first. A missing
// directory yields an empty list (not an error).
func (s *FileStore) List() ([]korai.Session, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	var sessions []korai.Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), fileExt) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), fileExt)
		sess, err := s.Load(id)
		if err != nil {
			continue // skip unreadable/corrupt files rather than failing the list
		}
		sessions = append(sessions, sess)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Updated.After(sessions[j].Updated) })
	return sessions, nil
}

// Delete removes the session file with the given id. A missing file is not an
// error (delete is idempotent), which lets a sync tombstone apply cleanly even
// if the session was never present locally. It is not part of the Store
// interface; callers that need it (the sync client applying tombstones) type-
// assert for it.
func (s *FileStore) Delete(id string) error {
	if err := os.Remove(s.path(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("deleting session %s: %w", id, err)
	}
	return nil
}

// Latest returns the most recently modified session for cwd, if any.
func (s *FileStore) Latest(cwd string) (korai.Session, bool, error) {
	sessions, err := s.List()
	if err != nil {
		return korai.Session{}, false, err
	}
	for _, sess := range sessions {
		if sess.CWD == cwd {
			return sess, true, nil
		}
	}
	return korai.Session{}, false, nil
}

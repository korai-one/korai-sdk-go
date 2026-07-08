package synchub

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/korai-one/korai-sdk-go/session"
)

// defaultInterval is the background poll cadence when none is configured.
const defaultInterval = 30 * time.Second

// minInterval clamps overly aggressive poll intervals.
const minInterval = 5 * time.Second

// Config is the resolved runtime configuration for the sync client. The zero
// value is disabled. Key is the 32-byte content key K_folder; SyncID is the
// opaque bearer namespace handle sent as the Authorization token.
type Config struct {
	Enabled    bool
	URL        string
	SyncID     string
	Key        []byte
	Interval   time.Duration
	CursorPath string
}

// FileSettings mirrors the optional "sync" block in ~/.korai/settings.json. The
// content key is deliberately NOT read from settings — it comes only from the
// KORAI_SYNC_KEY env var or the ~/.korai/sync.key file (see
// session.LoadContentKey) so a shared/committed settings file never carries key
// material.
//
// SyncID is retained for backward compatibility but is ignored: the namespace
// bearer is derived from the content key (session.DeriveSyncID) so any device
// holding the same key targets the same namespace with no manual configuration.
type FileSettings struct {
	Enabled  bool   `json:"enabled,omitempty"`
	URL      string `json:"url,omitempty"`
	SyncID   string `json:"syncId,omitempty"`   // deprecated: ignored (sync_id is derived from the key)
	Interval string `json:"interval,omitempty"` // Go duration, e.g. "30s"
}

// ErrIncomplete reports that sync was switched on but is missing a required
// value (URL or content key). Resolve returns a disabled Config with this error
// so the caller can warn and continue running without sync.
var ErrIncomplete = errors.New("sync enabled but not fully configured")

// Resolve builds a Config from the settings-file block overlaid by environment
// variables (env wins), plus the content key from session.LoadContentKey. home
// locates both the key file and the sync cursor.
//
// Enable signal: KORAI_SYNC_ENABLED (truthy) overrides fs.Enabled. When the
// resulting signal is off, Resolve returns a disabled Config and a nil error.
// When it is on but a required value is missing or malformed, Resolve returns a
// disabled Config and a non-nil error (ErrIncomplete or a parse error).
func Resolve(home string, fs FileSettings) (Config, error) {
	enabled := fs.Enabled
	if v, ok := lookupBool("KORAI_SYNC_ENABLED"); ok {
		enabled = v
	}
	if !enabled {
		return Config{}, nil
	}

	url := firstNonEmpty(strings.TrimSpace(os.Getenv("KORAI_SYNC_URL")), fs.URL)

	interval := defaultInterval
	if raw := firstNonEmpty(strings.TrimSpace(os.Getenv("KORAI_SYNC_INTERVAL")), fs.Interval); raw != "" {
		d, err := parseInterval(raw)
		if err != nil {
			return Config{}, fmt.Errorf("sync interval %q: %w", raw, err)
		}
		interval = d
	}
	if interval < minInterval {
		interval = minInterval
	}

	key, ok, err := session.LoadContentKey(home)
	if err != nil {
		return Config{}, fmt.Errorf("loading sync content key: %w", err)
	}

	if url == "" || !ok {
		return Config{}, fmt.Errorf("%w (url=%t key=%t)", ErrIncomplete, url != "", ok)
	}

	// The namespace bearer is derived from the key, not configured: every device
	// with the same K_folder resolves the same sync_id.
	return Config{
		Enabled:    true,
		URL:        strings.TrimRight(url, "/"),
		SyncID:     session.DeriveSyncID(key),
		Key:        key,
		Interval:   interval,
		CursorPath: filepath.Join(home, ".korai", "sync-cursor"),
	}, nil
}

// parseInterval accepts a Go duration ("30s") or a bare integer count of
// seconds ("30").
func parseInterval(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, errors.New("want a duration like 30s or an integer of seconds")
	}
	return time.Duration(n) * time.Second, nil
}

// lookupBool reads an optional boolean env var. ok is false when the var is
// unset, so an absent var does not override a settings-file value.
func lookupBool(name string) (val, ok bool) {
	raw, present := os.LookupEnv(name)
	if !present || strings.TrimSpace(raw) == "" {
		return false, false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, false
	}
	return v, true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

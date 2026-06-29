package korai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Local-worker discovery.
//
// A Korai worker running in local mode advertises itself by writing a small
// JSON file to a well-known path on startup. This lets a co-located program
// (the CLI, a desktop app, any SDK consumer) find the worker and route
// inference straight to it via WithLocalWorker — no orchestrator, no network.
// The advertisement is written by the worker (cmd/worker in the korai repo);
// this is the reader half.

// LocalWorkerInfo is a local worker's self-advertisement. The JSON tags are the
// cross-component contract with the worker that writes the file — keep them
// stable.
type LocalWorkerInfo struct {
	// URL is the worker's loopback base URL, e.g. http://127.0.0.1:54321.
	URL string `json:"url"`
	// PID is the worker process id (diagnostics only).
	PID int `json:"pid,omitempty"`
	// Models lists the canonical model ids the worker hosts.
	Models []string `json:"models,omitempty"`
	// Started is when the worker began listening.
	Started time.Time `json:"started,omitempty"`
}

// localWorkerProbeTimeout bounds the health probe so discovery never stalls.
const localWorkerProbeTimeout = time.Second

// LocalWorkerAdvertPath returns the well-known advertisement file,
// ~/.korai/local-worker.json, or "" when the home directory is unknown.
func LocalWorkerAdvertPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".korai", "local-worker.json")
}

// ReadLocalWorker loads the advertisement file without contacting the worker.
// It returns ok=false when the file is absent, unreadable, or malformed.
func ReadLocalWorker() (LocalWorkerInfo, bool) {
	path := LocalWorkerAdvertPath()
	if path == "" {
		return LocalWorkerInfo{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return LocalWorkerInfo{}, false
	}
	var info LocalWorkerInfo
	if err := json.Unmarshal(data, &info); err != nil || strings.TrimSpace(info.URL) == "" {
		return LocalWorkerInfo{}, false
	}
	return info, true
}

// DiscoverLocalWorker returns a reachable local worker, if one is advertised.
// It reads the advertisement file and confirms liveness via the worker's
// /health endpoint, so a stale advert (worker exited, or its port was reused)
// yields ok=false and the caller can fall back to the network.
func DiscoverLocalWorker(ctx context.Context) (LocalWorkerInfo, bool) {
	info, ok := ReadLocalWorker()
	if !ok {
		return LocalWorkerInfo{}, false
	}
	if !localWorkerHealthy(ctx, info.URL) {
		return LocalWorkerInfo{}, false
	}
	return info, true
}

// localWorkerHealthy reports whether baseURL/health answers 200 with an ok
// status. Any transport error or non-ok body means the worker is not usable.
func localWorkerHealthy(ctx context.Context, baseURL string) bool {
	ctx, cancel := context.WithTimeout(ctx, localWorkerProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return strings.Contains(string(body), `"ok"`)
}

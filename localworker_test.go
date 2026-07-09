package korai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func setLocalWorkerHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir) // Windows
	t.Setenv("HOME", dir)        // Unix
	return dir
}

func writeLocalWorkerAdvert(t *testing.T, home, url string) {
	t.Helper()
	korDir := filepath.Join(home, ".korai")
	if err := os.MkdirAll(korDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(LocalWorkerInfo{URL: url, PID: 99, Models: []string{"gemma"}})
	if err := os.WriteFile(filepath.Join(korDir, "local-worker.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func localWorkerHealthServer(t *testing.T, ok bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDiscoverLocalWorkerHealthy: a fresh advert + a healthy worker is found.
func TestDiscoverLocalWorkerHealthy(t *testing.T) {
	home := setLocalWorkerHome(t)
	srv := localWorkerHealthServer(t, true)
	writeLocalWorkerAdvert(t, home, srv.URL)

	info, ok := DiscoverLocalWorker(context.Background())
	if !ok {
		t.Fatal("DiscoverLocalWorker ok=false for a healthy advertised worker")
	}
	if info.URL != srv.URL {
		t.Errorf("url = %q, want %q", info.URL, srv.URL)
	}
}

// TestDiscoverLocalWorkerStale: an advert whose worker fails the probe is not
// returned, so callers fall back to the network.
func TestDiscoverLocalWorkerStale(t *testing.T) {
	home := setLocalWorkerHome(t)
	srv := localWorkerHealthServer(t, false)
	writeLocalWorkerAdvert(t, home, srv.URL)

	if _, ok := DiscoverLocalWorker(context.Background()); ok {
		t.Error("DiscoverLocalWorker should reject an unhealthy worker")
	}
}

// TestDiscoverLocalWorkerAbsent: no advert means no local worker.
func TestDiscoverLocalWorkerAbsent(t *testing.T) {
	setLocalWorkerHome(t)
	if _, ok := DiscoverLocalWorker(context.Background()); ok {
		t.Error("DiscoverLocalWorker should be ok=false with no advert file")
	}
}

// TestWithLocalWorkerClearsAuth: WithLocalWorker sets the base URL and drops any
// API key (local workers need no credentials).
func TestWithLocalWorkerClearsAuth(t *testing.T) {
	c := New(WithAPIKey("kfid_secret"), WithLocalWorker("http://127.0.0.1:7000/"))
	if c.BaseURL() != "http://127.0.0.1:7000" {
		t.Errorf("baseURL = %q, want trailing slash trimmed", c.BaseURL())
	}
	if c.APIKey() != "" {
		t.Errorf("apiKey = %q, want cleared for local worker", c.APIKey())
	}
}

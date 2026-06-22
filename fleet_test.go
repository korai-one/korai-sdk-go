package korai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const manifestBody = `{
  "schema_version": 1,
  "version": 7,
  "published_at": "2026-06-19T00:00:00Z",
  "models": {
    "gemma-4-31b-thinking": {
      "creator": "Google",
      "family": "gemma",
      "context_len": 32768,
      "roles": ["deep"],
      "variants": {
        "4bit": {"hf_repo": "org/repo", "size_gb": 18, "min_vram_gb": 24, "backend": "mlx"}
      }
    }
  },
  "deprecated": ["old-model"]
}`

func TestGetFleetManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/fleet/manifest" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(manifestBody))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	m, err := cli.GetFleetManifest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != 7 {
		t.Fatalf("version = %d", m.Version)
	}
	model, ok := m.Models["gemma-4-31b-thinking"]
	if !ok {
		t.Fatalf("model missing: %#v", m.Models)
	}
	if model.Family != "gemma" {
		t.Fatalf("family = %q", model.Family)
	}
	if model.Variants["4bit"].Backend != "mlx" {
		t.Fatalf("backend = %q", model.Variants["4bit"].Backend)
	}
	if len(m.Deprecated) != 1 || m.Deprecated[0] != "old-model" {
		t.Fatalf("deprecated = %v", m.Deprecated)
	}
}

func TestGetFleetStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":1,"version":7,"models":3,"tiers":4,"api_tiers":2,"deprecated":1}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	s, err := cli.GetFleetStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Models != 3 || s.Tiers != 4 {
		t.Fatalf("stats = %#v", s)
	}
}

package korai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestModesIncludesMax(t *testing.T) {
	var found bool
	for _, m := range Modes {
		if m == ModeMax {
			found = true
		}
	}
	if !found {
		t.Fatalf("ModeMax not present in Modes: %#v", Modes)
	}
	// Order: auto, fast, balanced, deep, max.
	want := []Mode{ModeAuto, ModeFast, ModeBalanced, ModeDeep, ModeMax}
	if len(Modes) != len(want) {
		t.Fatalf("Modes = %#v, want %#v", Modes, want)
	}
	for i := range want {
		if Modes[i] != want[i] {
			t.Fatalf("Modes[%d] = %q, want %q", i, Modes[i], want[i])
		}
	}
}

func TestSolvePostsAndParsesRunID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/solve" {
			http.NotFound(w, r)
			return
		}
		var req ChatRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if req.Model != "max" {
			t.Fatalf("expected model=max, got %q", req.Model)
		}
		if req.Stream {
			t.Fatalf("expected stream=false")
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"run_id":"run-123","status":"running","model":"max"}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL), WithMaxRetries(0))
	run, err := cli.Solve(context.Background(), ChatRequest{
		Model:    "auto", // overridden to max
		Messages: []Message{{Role: "user", Content: "prove it"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.RunID != "run-123" {
		t.Fatalf("run_id = %q", run.RunID)
	}
	if run.Status != "running" {
		t.Fatalf("status = %q", run.Status)
	}
}

func TestSolveRequiresMessages(t *testing.T) {
	cli := New()
	_, err := cli.Solve(context.Background(), ChatRequest{})
	if err == nil {
		t.Fatal("expected error for empty messages")
	}
}

func TestGetSolveRunParsesDoneWithUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/solve/run-123" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{
			"run_id":"run-123",
			"status":"done",
			"model":"max",
			"result":"the answer is 42",
			"usage":{"prompt_tokens":120,"completion_tokens":980}
		}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL), WithMaxRetries(0))
	run, err := cli.GetSolveRun(context.Background(), "run-123")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "done" {
		t.Fatalf("status = %q", run.Status)
	}
	if run.Result != "the answer is 42" {
		t.Fatalf("result = %q", run.Result)
	}
	if run.PromptTokens != 120 || run.CompletionTokens != 980 {
		t.Fatalf("usage = (%d,%d), want (120,980)", run.PromptTokens, run.CompletionTokens)
	}
}

func TestGetSolveRunNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"message":"no such run","type":"not_found"}}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL), WithMaxRetries(0))
	_, err := cli.GetSolveRun(context.Background(), "missing")
	if !IsNotFoundStatus(err) {
		t.Fatalf("expected 404 APIError, got %v", err)
	}
}

func TestSolveAndWaitPollsRunningToDone(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/solve":
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"run_id":"run-9","status":"running","model":"max"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/solve/run-9":
			n := atomic.AddInt32(&polls, 1)
			if n < 2 {
				w.Write([]byte(`{"run_id":"run-9","status":"running","model":"max"}`))
				return
			}
			w.Write([]byte(`{"run_id":"run-9","status":"done","model":"max","result":"final","usage":{"prompt_tokens":5,"completion_tokens":7}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL), WithMaxRetries(0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run, err := cli.SolveAndWait(ctx, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hard problem"}},
	}, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "done" {
		t.Fatalf("status = %q", run.Status)
	}
	if run.Result != "final" {
		t.Fatalf("result = %q", run.Result)
	}
	if run.CompletionTokens != 7 {
		t.Fatalf("completion_tokens = %d", run.CompletionTokens)
	}
	if atomic.LoadInt32(&polls) < 2 {
		t.Fatalf("expected >= 2 polls, got %d", polls)
	}
}

func TestSolveAndWaitRespectsContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"run_id":"run-x","status":"running","model":"max"}`))
		default:
			// Never finishes.
			w.Write([]byte(`{"run_id":"run-x","status":"running","model":"max"}`))
		}
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL), WithMaxRetries(0))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := cli.SolveAndWait(ctx, ChatRequest{
		Messages: []Message{{Role: "user", Content: "loop forever"}},
	}, 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected context deadline error")
	}
}

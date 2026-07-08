package korai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// SolveRun is the state of a durable AB-MCTS deep-search run (the "max"
// mode). The orchestrator runs the search detached from the request
// connection: Client.Solve starts one and returns immediately with a
// RunID + Status; Client.GetSolveRun polls its progress until Status is
// "done" or "error".
//
// PromptTokens / CompletionTokens are flattened from the wire's nested
// "usage" object (see UnmarshalJSON) so callers reach token accounting
// without an extra hop through a Usage value. They are zero until the
// run completes.
type SolveRun struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
	Model  string `json:"model"`
	// Result holds the final answer text once Status == "done". Empty
	// while the run is still in flight or on error.
	Result string `json:"result"`
	// Error carries a server-side failure message when Status == "error".
	Error string `json:"error"`
	// PromptTokens / CompletionTokens are mapped from the nested
	// "usage":{"prompt_tokens":n,"completion_tokens":m} wire object.
	PromptTokens     int `json:"-"`
	CompletionTokens int `json:"-"`
}

// UnmarshalJSON decodes the SolveRun wire shape, lifting the nested
// usage object onto the flat PromptTokens / CompletionTokens fields.
// The wire shape is:
//
//	{"run_id":"...","status":"...","model":"...","result":"...",
//	 "error":"...","usage":{"prompt_tokens":n,"completion_tokens":m}}
func (s *SolveRun) UnmarshalJSON(data []byte) error {
	var raw struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Result string `json:"result"`
		Error  string `json:"error"`
		Usage  struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.RunID = raw.RunID
	s.Status = raw.Status
	s.Model = raw.Model
	s.Result = raw.Result
	s.Error = raw.Error
	s.PromptTokens = raw.Usage.PromptTokens
	s.CompletionTokens = raw.Usage.CompletionTokens
	return nil
}

// Solve starts a durable AB-MCTS deep-search run. It forces
// req.Model = "max", POSTs the request to /v1/solve, and parses the
// 202 {run_id, status} acknowledgement into a SolveRun. The search
// itself runs detached from this connection — poll its progress with
// GetSolveRun, or use SolveAndWait to block until it finishes.
//
// A 503 storage_unavailable (the orchestrator has no Postgres to persist
// runs) surfaces as the SDK's normal *APIError, like any other failure.
func (c *Client) Solve(ctx context.Context, req ChatRequest) (*SolveRun, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("%w: messages must be non-empty", ErrInvalidConfig)
	}
	req.Model = string(ModeMax)
	req.Stream = false
	var out SolveRun
	if err := c.doRequest(ctx, http.MethodPost, "/v1/solve", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSolveRun fetches the current state of a solve run by id. A non-200
// response maps to the SDK's typed *APIError, exactly like ChatComplete
// and ListModels — e.g. errors.Is(err, ErrNotFound) for an unknown id.
func (c *Client) GetSolveRun(ctx context.Context, runID string) (*SolveRun, error) {
	if runID == "" {
		return nil, fmt.Errorf("%w: runID must be non-empty", ErrInvalidConfig)
	}
	var out SolveRun
	if err := c.doRequest(ctx, http.MethodGet, "/v1/solve/"+runID, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SolveAndWait starts a solve run and polls it until it reaches a
// terminal status ("done" or "error") or ctx is cancelled. The caller
// owns the deadline: pass a context.WithTimeout / WithDeadline to bound
// the wait. pollInterval is the gap between GetSolveRun calls; a value
// <= 0 defaults to one second.
//
// The returned *SolveRun is the last successfully polled state. A run
// that ends with Status == "error" is NOT turned into a Go error — the
// SolveRun is returned with its Error field set so the caller can read
// partial accounting; only transport/HTTP failures and ctx cancellation
// produce a non-nil error.
func (c *Client) SolveAndWait(ctx context.Context, req ChatRequest, pollInterval time.Duration) (*SolveRun, error) {
	run, err := c.Solve(ctx, req)
	if err != nil {
		return nil, err
	}
	if isTerminalSolveStatus(run.Status) {
		return run, nil
	}
	if pollInterval <= 0 {
		pollInterval = time.Second
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return run, ctx.Err()
		case <-ticker.C:
			latest, err := c.GetSolveRun(ctx, run.RunID)
			if err != nil {
				return run, err
			}
			run = latest
			if isTerminalSolveStatus(run.Status) {
				return run, nil
			}
		}
	}
}

// isTerminalSolveStatus reports whether a solve run has finished (either
// successfully or with an error) and no longer needs polling.
func isTerminalSolveStatus(status string) bool {
	return status == "done" || status == "error"
}

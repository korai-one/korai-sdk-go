package korai

import (
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

// retryTransport is an http.RoundTripper that retries transient failures
// (HTTP 408/409/429/5xx and connection errors) with exponential backoff +
// jitter, honoring Retry-After. It wraps the default client's transport so
// every request — generated-client and doRequest alike — retries uniformly.
// The request context bounds total wall-clock: when it's cancelled (e.g. the
// http.Client timeout fires), the backoff sleep aborts and the last error is
// returned.
type retryTransport struct {
	base       http.RoundTripper
	maxRetries int
}

func newRetryTransport(base http.RoundTripper, maxRetries int) *retryTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &retryTransport{base: base, maxRetries: maxRetries}
}

func (rt *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	var lastResp *http.Response
	var lastErr error

	for attempt := 0; ; attempt++ {
		// Reset the body for replay on each attempt. http.NewRequestWithContext
		// sets GetBody for in-memory bodies (the SDK marshals JSON to a
		// bytes.Reader), so retried writes resend their payload.
		if req.Body != nil && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}

		resp, err := rt.base.RoundTrip(req)
		if err != nil {
			lastErr = err
			if attempt >= rt.maxRetries || ctx.Err() != nil {
				return nil, err
			}
		} else {
			if resp.StatusCode < 400 || attempt >= rt.maxRetries || !isRetryableStatus(resp.StatusCode) {
				return resp, nil
			}
			lastResp = resp
		}

		delay := retryDelay(attempt, lastResp)
		// Drain + close the retryable response so the connection can be reused.
		if lastResp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(lastResp.Body, 1<<16))
			_ = lastResp.Body.Close()
			lastResp = nil
		}

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, ctx.Err()
		}
	}
}

// isRetryableStatus reports whether an HTTP status is worth retrying.
func isRetryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || // 408
		status == http.StatusConflict || // 409
		status == http.StatusTooManyRequests || // 429
		status >= 500
}

// retryDelay computes the backoff for attempt (0-based). Honors Retry-After
// (seconds) on the response when present; otherwise exponential (500ms base,
// 8s cap) with 50–100% jitter.
func retryDelay(attempt int, resp *http.Response) time.Duration {
	if resp != nil {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
				d := time.Duration(secs) * time.Second
				if d > 60*time.Second {
					d = 60 * time.Second
				}
				return d
			}
		}
	}
	exp := 500 * time.Millisecond * (1 << attempt)
	if exp > 8*time.Second {
		exp = 8 * time.Second
	}
	// 50–100% jitter.
	return exp/2 + time.Duration(rand.Int63n(int64(exp/2)+1))
}

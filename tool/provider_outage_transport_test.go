package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// outageTransport intercepts both readiness and model requests. No request can
// escape to a real endpoint, including an accidentally inherited configuration.
type outageTransport func(*http.Request) (*http.Response, error)

func (f outageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func outageResponse(r *http.Request, status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}
func installOutageTransport(t *testing.T, mode string) *atomic.Int32 {
	t.Helper()
	old := http.DefaultTransport
	calls := new(atomic.Int32)
	http.DefaultTransport = outageTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return outageResponse(r, 200, `{"data":{"limit_remaining":10}}`)
		}
		calls.Add(1)
		switch mode {
		case "refused":
			return nil, fmt.Errorf("dial tcp: connection refused")
		case "429":
			return outageResponse(r, 429, `{"error":"rate limit"}`)
		case "503":
			return outageResponse(r, 503, `{"error":"unavailable"}`)
		case "401":
			return outageResponse(r, 401, `{"error":"unauthorized"}`)
		case "timeout":
			return nil, context.DeadlineExceeded
		case "hang":
			<-r.Context().Done()
			return nil, r.Context().Err()
		case "malformed":
			return outageResponse(r, 200, `{broken`)
		case "empty":
			return outageResponse(r, 200, `{}`)
		case "recovered":
			return outageResponse(r, 200, decReply(settledAnswers))
		default:
			return nil, fmt.Errorf("unexpected fake outage mode %q", mode)
		}
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	return calls
}

var outageModes = []string{"refused", "429", "503", "timeout", "401", "malformed", "empty"}

package apierror

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFromMapsKnownErrors(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{context.DeadlineExceeded, http.StatusGatewayTimeout, CodeDeadlineExceeded},
		{context.Canceled, StatusClientClosedRequest, CodeCanceled},
		{errors.New("boom"), http.StatusInternalServerError, CodeInternal},
		{New(http.StatusNotFound, CodeNotFound, "nope"), http.StatusNotFound, CodeNotFound},
		{&http.MaxBytesError{Limit: 10}, http.StatusRequestEntityTooLarge, CodePayloadTooLarge},
	}
	for _, c := range cases {
		e := From(c.err)
		if e.Status != c.status || e.Code != c.code {
			t.Errorf("From(%v) = %d %s, want %d %s", c.err, e.Status, e.Code, c.status, c.code)
		}
	}
}

func TestInternalErrorsDoNotLeakCause(t *testing.T) {
	e := From(errors.New("pg: password authentication failed for user svc"))
	if e.Message != "an internal error occurred" {
		t.Fatalf("message leaked: %q", e.Message)
	}
	if !errors.Is(e, e.cause) {
		t.Fatal("cause must remain reachable for logging")
	}
}

func TestWriteSetsHeadersAndEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, New(http.StatusTooManyRequests, CodeRateLimited, "slow down").WithRetryAfter(7))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After %q", got)
	}
	if got := rec.Header().Get("X-Error-Code"); got != CodeRateLimited {
		t.Fatalf("X-Error-Code %q", got)
	}
	var body struct {
		Error Error `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != CodeRateLimited || !body.Error.Retryable || body.Error.RetryAfter != 7 {
		t.Fatalf("envelope %+v", body.Error)
	}
}

func TestRPCCodeMapping(t *testing.T) {
	if HTTPStatusForRPCCode("unavailable") != http.StatusServiceUnavailable {
		t.Fatal("unavailable")
	}
	if HTTPStatusForRPCCode("resource_exhausted") != http.StatusTooManyRequests {
		t.Fatal("resource_exhausted")
	}
}

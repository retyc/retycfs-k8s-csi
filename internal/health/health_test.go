package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func TestHandler_HealthzIsUnconditional(t *testing.T) {
	h := Handler(func(context.Context) error { return errors.New("backend down") }, nil)
	if code, body := get(t, h, "/healthz"); code != http.StatusOK || body != "ok" {
		t.Fatalf("/healthz = %d %q, want 200 ok even when not ready", code, body)
	}
}

func TestHandler_ReadyzReportsCheckerError(t *testing.T) {
	h := Handler(func(context.Context) error { return errors.New("backend down") }, nil)
	code, body := get(t, h, "/readyz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "backend down") {
		t.Fatalf("/readyz = %d %q, want 503 with the reason", code, body)
	}

	h = Handler(func(context.Context) error { return nil }, nil)
	if code, _ := get(t, h, "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200", code)
	}

	if code, _ := get(t, Handler(nil, nil), "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz with nil checker = %d, want 200", code)
	}
}

func TestHandler_MetricsOnlyWhenGiven(t *testing.T) {
	if code, _ := get(t, Handler(nil, nil), "/metrics"); code != http.StatusNotFound {
		t.Fatalf("/metrics without a handler = %d, want 404", code)
	}
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("up 1")) })
	if code, body := get(t, Handler(nil, metrics), "/metrics"); code != http.StatusOK || body != "up 1" {
		t.Fatalf("/metrics = %d %q, want the metrics handler's answer", code, body)
	}
}

// Package health serves the HTTP liveness/readiness endpoints of the driver, and its /metrics.
//
// The two endpoints deliberately mean different things. /healthz only says "the process is up
// and serving"; it backs the kubelet livenessProbe, and a failure there restarts the container.
// On the node plugin, liveness must never trip on a transient condition. /readyz backs the
// readinessProbe and runs the mode's Checker when there is one; the driver has none today (the
// identities show on their RetycIdentity, so that `helm install --wait` never waits for an approval).
package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"k8s.io/klog/v2"
)

// Checker reports why the plugin is not ready, or nil when it is.
type Checker func(ctx context.Context) error

// checkTimeout bounds a single /readyz evaluation so a hung dependency cannot pin the probe.
const checkTimeout = 5 * time.Second

// Handler returns the HTTP mux serving /healthz, /readyz and, when metrics is not nil, /metrics.
// ready may be nil, in which case /readyz always succeeds (there is nothing mode-specific to
// check).
func Handler(ready Checker, metrics http.Handler) http.Handler {
	mux := http.NewServeMux()
	if metrics != nil {
		mux.Handle("/metrics", metrics)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready != nil {
			ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
			defer cancel()
			if err := ready(ctx); err != nil {
				// kubelet's event only carries the status code; the reason has to be somewhere
				// `kubectl logs` shows it. One line per failed probe, i.e. only while broken.
				klog.Warningf("/readyz: not ready: %v", err)
				http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)

				return
			}
		}
		fmt.Fprintln(w, "ok")
	})

	return mux
}

// ListenAndServe serves handler on addr until ctx is done, then shuts down gracefully.
func ListenAndServe(ctx context.Context, addr string, handler http.Handler) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	klog.Infof("HTTP endpoints listening on %s (/healthz, /readyz, /metrics)", listener.Addr())
	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

// Package rest serves the CLI-level REST API: deployment/configuration
// endpoints that the agent-level rest package does not own — channel
// connection control and settings access. It lives under cmd/cli
// because it operates on CLI concepts (settings.json via cmd/cli/config,
// the process-level channel manager in cmd/cli/server); the agent-level
// API (sessions/chat/approve) stays in the top-level rest package.
package rest

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Register mounts the CLI-level API on mux. mgr is the process-level
// feishu connection manager (never nil in practice — RunChannels always
// creates one; guarded anyway).
func Register(mux *http.ServeMux, mgr FeishuChannel) {
	mux.HandleFunc("GET /api/channels/feishu/status", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu channel not configured"})
			return
		}
		writeJSON(w, http.StatusOK, mgr.Status())
	})
	mux.HandleFunc("POST /api/channels/feishu/connect", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu channel not configured"})
			return
		}
		qrCh := make(chan string, 1)
		registration, err := mgr.ConnectAsync(func(url string, _ int) {
			select {
			case qrCh <- url:
			default:
			}
		})
		if err != nil {
			// Machine lock held by another instance — fail-fast surfaced
			// to the caller.
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if !registration {
			writeJSON(w, http.StatusOK, mgr.Status())
			return
		}
		// Registration flow: wait for the QR URL (registration start is
		// fast; the request context cancelling here does NOT cancel the
		// registration — it runs on the process context). The image is
		// read from the cache (written by the onQR callback).
		select {
		case url := <-qrCh:
			_, img := mgr.QR()
			writeJSON(w, http.StatusAccepted, map[string]any{
				"status":         "registration",
				"qr_url":         url,
				"qr_img_base64": img,
			})
		case <-r.Context().Done():
			// Client went away before the URL arrived; the registration
			// itself continues on the process context.
		}
	})
	mux.HandleFunc("POST /api/channels/feishu/disconnect", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu channel not configured"})
			return
		}
		mgr.Disconnect()
		writeJSON(w, http.StatusOK, mgr.Status())
	})
	// Re-fetch the registration QR (URL + base64 PNG) after a refresh —
	// POST /connect is idempotent while registering and does not re-issue
	// it.
	mux.HandleFunc("GET /api/channels/feishu/qr", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu channel not configured"})
			return
		}
		// Guard on the live phase, not just the cache file: a QR file
		// left over from a pre-restart registration must not be served.
		if mgr.Status().Phase != FeishuRegistering {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no QR registration in flight"})
			return
		}
		url, img := mgr.QR()
		if url == "" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no QR registration in flight"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"qr_url":       url,
			"qr_img_base64": img,
		})
	})

	// Configuration endpoints (generic settings domain).
	registerSettings(mux, mgr)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("cli-rest: json encode failed", "error", err)
	}
}

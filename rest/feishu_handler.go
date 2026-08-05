package rest

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// WithFeishuManager attaches the process-level feishu connection manager
// (nil when feishu is not configured). Enables:
//
//	GET  /api/channels/feishu/status   — query connection state (page refresh)
//	POST /api/channels/feishu/connect  — trigger connect / QR registration
//
// Routes are registered unconditionally in Handler.Register; handlers
// 404 when the manager is nil.
func (h *Handler) WithFeishuManager(mgr FeishuChannel) *Handler {
	h.feishu = mgr
	return h
}

// handleFeishuStatus reports the feishu connection state. Frontends call
// this on page load (after a refresh) — the connection is a
// process-level daemon, so the answer is authoritative regardless of
// which tab asks.
func (h *Handler) handleFeishuStatus(w http.ResponseWriter, r *http.Request) {
	if h.feishu == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu channel not configured"})
		return
	}
	writeJSON(w, http.StatusOK, h.feishu.Status())
}

// handleFeishuConnect triggers the feishu connection flow.
//
//   - credentials present  → connection starts on the process context,
//     200 with the current status
//   - no credentials       → QR registration starts (asynchronously, on
//     the process context), 202 with the QR URL; the handler waits only
//     for the registration to START (the URL), never for the user to
//     scan — the frontend renders the QR and polls /status until
//     connected
//   - another instance holds the machine lock → 409
func (h *Handler) handleFeishuConnect(w http.ResponseWriter, r *http.Request) {
	if h.feishu == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu channel not configured"})
		return
	}
	qrCh := make(chan string, 1)
	registration, err := h.feishu.ConnectAsync(func(url string, _ int) {
		select {
		case qrCh <- url:
		default:
		}
	})
	if err != nil {
		// Machine lock held by another instance — fail-fast surfaced to
		// the caller.
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if !registration {
		writeJSON(w, http.StatusOK, h.feishu.Status())
		return
	}
	// Registration flow: wait for the QR URL (registration start is
	// fast; the request context cancelling here does NOT cancel the
	// registration — it runs on the process context).
	select {
	case url := <-qrCh:
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status": "registration",
			"qr_url": url,
		})
	case <-r.Context().Done():
		// Client went away before the URL arrived; the registration
		// itself continues on the process context.
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("rest: json encode failed", "error", err)
	}
}

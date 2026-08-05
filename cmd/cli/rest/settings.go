package rest

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Settings routes under /api/settings/ — the generic configuration
// domain. Currently only channels/feishu; future config items
// (provider, embedding, ...) register here too.
//
//	GET /api/settings/channels/feishu — current feishu configuration
//	PUT /api/settings/channels/feishu — store it (next connect applies)
func registerSettings(mux *http.ServeMux, mgr FeishuChannel) {
	mux.HandleFunc("GET /api/settings/channels/feishu", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu channel not configured"})
			return
		}
		appID, secret := mgr.Credentials()
		writeJSON(w, http.StatusOK, map[string]any{
			"app_id":     appID,
			"app_secret": maskSecret(secret),
		})
	})
	mux.HandleFunc("PUT /api/settings/channels/feishu", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu channel not configured"})
			return
		}
		var req struct {
			AppID     string `json:"app_id"`
			AppSecret string `json:"app_secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
		if err := mgr.SetCredentials(req.AppID, req.AppSecret); err != nil {
			if errors.Is(err, ErrFeishuRegistrationInFlight) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"saved": true})
	})
	// Clear the feishu credentials — the frontend "re-register" flow is
	// DELETE + POST /connect (with no credentials left, connect runs QR
	// registration). A running connection keeps working until the next
	// connect (credentials and connection are separate).
	mux.HandleFunc("DELETE /api/settings/channels/feishu", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu channel not configured"})
			return
		}
		if err := mgr.ClearCredentials(); err != nil {
			if errors.Is(err, ErrFeishuRegistrationInFlight) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
	})
}

// maskSecret masks a secret for display: "****" + last 4 chars (or
// "****" alone when shorter). The full value is never returned to the
// frontend.
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= 4 {
		return "****"
	}
	return "****" + string(runes[len(runes)-4:])
}

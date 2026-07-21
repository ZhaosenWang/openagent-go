package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"
	"github.com/mdp/qrterminal/v3"
)

// FeishuCredentials holds resolved app credentials.
type FeishuCredentials struct {
	AppID     string
	AppSecret string
}

// ResolveFeishuCredentials obtains Feishu app credentials. Resolution order:
//  1. settings.json channels.feishu (already loaded into cfg — checked by caller)
//  2. ~/.openagent/data/feishu_app.json (persisted from previous registration)
//  3. QR code registration flow (blocks until user authorizes)
func ResolveFeishuCredentials(ctx context.Context) (FeishuCredentials, error) {
	// Try persisted file first.
	creds, ok := loadFeishuAppFile()
	if ok {
		fmt.Fprintln(os.Stderr, "feishu: using persisted credentials from ~/.openagent/data/feishu_app.json")
		return creds, nil
	}

	// QR code registration.
	fmt.Fprintln(os.Stderr,"feishu: no credentials found. Starting one-click app registration...")
	return registerFeishuApp(ctx)
}

// ── Persisted credential file ──

func feishuAppPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".openagent", "data", "feishu_app.json")
}

func loadFeishuAppFile() (FeishuCredentials, bool) {
	data, err := os.ReadFile(feishuAppPath())
	if err != nil {
		return FeishuCredentials{}, false
	}
	var c FeishuCredentials
	if err := json.Unmarshal(data, &c); err != nil {
		return FeishuCredentials{}, false
	}
	if c.AppID == "" || c.AppSecret == "" {
		return FeishuCredentials{}, false
	}
	fmt.Fprintf(os.Stderr, "feishu: using persisted credentials from %s\n", feishuAppPath())
	return c, true
}

func saveFeishuAppFile(c FeishuCredentials) {
	p := feishuAppPath()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "feishu: failed to create credential directory: %v\n", err)
		return
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "feishu: failed to marshal credentials: %v\n", err)
		return
	}
	if err := os.WriteFile(p, data, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "feishu: failed to save credentials to %s: %v\n", p, err)
	}
}

// ── Registration flow ──

func registerFeishuApp(ctx context.Context) (FeishuCredentials, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	result, err := registration.RegisterApp(ctx, &registration.Options{
		AppPreset: &registration.AppPreset{
			Name: "openagent-bot",
			Desc: "AI coding agent powered by openagent-go",
		},
		Addons: &registration.AppAddons{
			Scopes: registration.AppAddonsScopes{
				Tenant: []string{
					"im:message",
					"im:message:send_as_bot",
				},
			},
			Events: registration.AppAddonsEvents{
				Items: registration.AppAddonsEventItems{
					Tenant: []string{
						"im.message.receive_v1",
					},
				},
			},
		},
		OnQRCode: func(info *registration.QRCodeInfo) {
			// Render QR code in terminal.
			fmt.Fprintln(os.Stderr)
			qrterminal.GenerateHalfBlock(info.URL, qrterminal.L, os.Stderr)
			fmt.Fprintln(os.Stderr)
			fmt.Fprintf(os.Stderr, "  Open this link in Feishu: %s\n", info.URL)
			fmt.Fprintf(os.Stderr, "  (expires in %d seconds)\n", info.ExpireIn)
			fmt.Fprintln(os.Stderr)
		},
		OnStatusChange: func(info *registration.StatusChangeInfo) {
			// Quiet polling; no console spam.
			_ = info
		},
	})
	if err != nil {
		return FeishuCredentials{}, fmt.Errorf("feishu registration: %w", err)
	}

	creds := FeishuCredentials{
		AppID:     result.ClientID,
		AppSecret: result.ClientSecret,
	}
	saveFeishuAppFile(creds)

	fmt.Fprintf(os.Stderr, "feishu: app created — App ID: %s\n", creds.AppID)
	fmt.Fprintln(os.Stderr, "feishu: credentials saved. Add to settings.json to skip registration next time:")
	fmt.Fprintf(os.Stderr, "  \"channels\": { \"feishu\": { \"app_id\": \"%s\", \"app_secret\": \"%s\" } }\n",
		creds.AppID, creds.AppSecret)

	return creds, nil
}

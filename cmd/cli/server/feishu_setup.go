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
//  2. $profile/channel/feishu/feishu_app.json (persisted from previous registration)
//  3. QR code registration flow (blocks until user authorizes)
//
// onQR, when non-nil, receives the registration QR info (an API-driven
// caller renders it for the user instead of the terminal); when nil the
// QR is printed to stderr.
func ResolveFeishuCredentials(ctx context.Context, profiles string, onQR func(url string, expireIn int)) (FeishuCredentials, error) {
	// Try persisted file first.
	creds, ok := loadFeishuAppFile(profiles)
	if ok {
		fmt.Fprintln(os.Stderr, "feishu: using persisted credentials from "+feishuAppPath(profiles))
		return creds, nil
	}

	// QR code registration.
	fmt.Fprintln(os.Stderr, "feishu: no credentials found. Starting one-click app registration...")
	return registerFeishuApp(ctx, profiles, onQR)
}

// ── Persisted credential file ──

// feishuAppPath returns the credential file path under the profile's
// channel directory. Credentials travel with the profile — different
// profiles are different bots, and the channel lock lives next to them.
func feishuAppPath(profiles string) string {
	return filepath.Join(resolveProfilesDir(profiles), "channel", "feishu", "feishu_app.json")
}

// legacyFeishuAppPath is the pre-2026-08 location; read for migration only.
func legacyFeishuAppPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".openagent", "data", "feishu_app.json")
}

func loadFeishuAppFile(profiles string) (FeishuCredentials, bool) {
	p := feishuAppPath(profiles)
	data, err := os.ReadFile(p)
	if err != nil {
		// Migration: the legacy file (~/.openagent/data/feishu_app.json)
		// predates profile-scoped credentials. Copy it into the profile
		// location (leaving the old file untouched) so existing users
		// keep working without re-registering.
		if legacy, lerr := os.ReadFile(legacyFeishuAppPath()); lerr == nil {
			var c FeishuCredentials
			if json.Unmarshal(legacy, &c) == nil && c.AppID != "" && c.AppSecret != "" {
				saveFeishuAppFile(profiles, c)
				return c, true
			}
		}
		return FeishuCredentials{}, false
	}
	var c FeishuCredentials
	if err := json.Unmarshal(data, &c); err != nil {
		return FeishuCredentials{}, false
	}
	if c.AppID == "" || c.AppSecret == "" {
		return FeishuCredentials{}, false
	}
	return c, true
}

func saveFeishuAppFile(profiles string, c FeishuCredentials) {
	p := feishuAppPath(profiles)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "feishu: failed to create credential directory: %v\n", err)
		return
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "feishu: failed to marshal credentials: %v\n", err)
		return
	}
	// Atomic write (temp file + rename): a crash or a concurrent writer
	// mid-write can never leave a truncated credential file that would
	// silently break the next startup.
	tmp, err := os.CreateTemp(filepath.Dir(p), "feishu_app-*.tmp")
	if err != nil {
		fmt.Fprintf(os.Stderr, "feishu: failed to create temp credential file: %v\n", err)
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		fmt.Fprintf(os.Stderr, "feishu: failed to chmod credentials: %v\n", err)
		return
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		fmt.Fprintf(os.Stderr, "feishu: failed to write credentials: %v\n", err)
		return
	}
	if err := tmp.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "feishu: failed to close credentials: %v\n", err)
		return
	}
	if err := os.Rename(tmpName, p); err != nil {
		fmt.Fprintf(os.Stderr, "feishu: failed to save credentials to %s: %v\n", p, err)
	}
}

// ── Registration flow ──

func registerFeishuApp(ctx context.Context, profiles string, onQR func(url string, expireIn int)) (FeishuCredentials, error) {
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
			if onQR != nil {
				// API-driven caller renders the QR for the user.
				onQR(info.URL, info.ExpireIn)
				return
			}
			// Terminal mode: render the QR code inline.
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
	saveFeishuAppFile(profiles, creds)

	fmt.Fprintf(os.Stderr, "feishu: app created — App ID: %s\n", creds.AppID)
	fmt.Fprintln(os.Stderr, "feishu: credentials saved. Add to settings.json to skip registration next time:")
	fmt.Fprintf(os.Stderr, "  \"channels\": { \"feishu\": { \"app_id\": \"%s\", \"app_secret\": \"%s\" } }\n",
		creds.AppID, creds.AppSecret)

	return creds, nil
}

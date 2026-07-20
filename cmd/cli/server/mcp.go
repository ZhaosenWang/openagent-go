package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/mcp"

	"github.com/yusheng-g/openagent-go/cmd/cli/config"
)

// connectMcpFromConfig connects to all configured MCP servers and returns
// their tools plus a cleanup function that closes all sessions.
// Failed connections are logged but not fatal.
func connectMcpFromConfig(ctx context.Context, servers map[string]config.McpServerConfig) ([]openagent.Tool, func()) {
	if len(servers) == 0 {
		return nil, func() {}
	}

	client := mcp.NewClient("openagent-cli", "1.0.0")
	var tools []openagent.Tool
	var sessions []*mcp.Session

	for name, s := range servers {
		sess, err := connectMcpOne(ctx, client, s)
		if err != nil {
			slog.Warn("mcp connect failed", "name", name, "error", err)
			continue
		}
		list, err := sess.Tools(ctx)
		if err != nil {
			sess.Close()
			slog.Warn("mcp list tools failed", "name", name, "error", err)
			continue
		}
		sessions = append(sessions, sess)
		tools = append(tools, list...)
		slog.Info("mcp connected", "name", name, "tools", len(list))
	}

	return tools, func() {
		for _, s := range sessions {
			s.Close()
		}
	}
}

func connectMcpOne(ctx context.Context, client *mcp.Client, cfg config.McpServerConfig) (*mcp.Session, error) {
	switch cfg.Type {
	case "http", "sse":
		if cfg.URL == "" {
			return nil, fmt.Errorf("missing url")
		}
		return client.ConnectHTTP(ctx, cfg.URL)
	default:
		if cfg.Command == "" {
			return nil, fmt.Errorf("missing command")
		}
		cmd := cfg.Command
		if strings.HasPrefix(cmd, "./") || strings.HasPrefix(cmd, "../") {
			if abs, err := filepath.Abs(cmd); err == nil {
				cmd = abs
			}
		}
		var env []string
		for k, v := range cfg.Env {
			env = append(env, k+"="+v)
		}
		env = append(env, os.Environ()...)
		return client.ConnectStdioWithEnv(ctx, cmd, cfg.Args, env)
	}
}

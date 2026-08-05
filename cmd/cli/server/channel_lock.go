package server

import (
	"os"
	"path/filepath"

	"github.com/yusheng-g/openagent-go/utils"
)

// ChannelLock is the machine-level exclusive lock for a named IM channel
// (feishu, ...). Feishu's WebSocket keeps ONE active connection per app —
// a second instance would silently steal events from the first (the old
// connection stops receiving with no error, looking "up" but dead). The
// lock covers the whole channel lifecycle (credential registration +
// WebSocket), so two concurrently started servers cannot race the shared
// credential file or the connection.
//
// The lock file lives under the channel's profile directory
// ($profile/channel/<name>/), never CWD: the server may be started from
// any directory, and the lock is the same one regardless. Different
// profiles = different locks = independent channel instances (the
// multi-bot deployment model). The underlying flock is released by the
// kernel when the process dies (even a crash), so there is no
// stale-lock problem.
type ChannelLock struct {
	*utils.FileLock
}

// AcquireChannelLock takes the machine-level lock for the named channel
// under the given profile directory. Returns an error when another
// instance holds it.
func AcquireChannelLock(profiles, name string) (*ChannelLock, error) {
	p := filepath.Join(resolveProfilesDir(profiles), "channel", name, name+".lock")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	l, err := utils.AcquireFileLock(p)
	if err != nil {
		return nil, err
	}
	l.WritePID()
	return &ChannelLock{FileLock: l}, nil
}

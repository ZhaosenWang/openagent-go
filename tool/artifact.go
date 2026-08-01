package tool

import (
	"os"
	"path/filepath"
)

// ArtifactRoot returns the platform-appropriate artifact directory.
// Linux/macOS: /tmp/openagent
// Windows:     %TEMP%\openagent
//
// Tool results exceeding a size threshold can be saved here by hooks and
// referenced in the tool result summary. The system tmp cleaner reclaims
// the space eventually, so artifacts are best-effort persistent.
func ArtifactRoot() string {
	return filepath.Join(os.TempDir(), "openagent")
}

package bge

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/yalue/onnxruntime_go"
)

// The ONNX Runtime native library (libonnxruntime.so on linux/darwin,
// onnxruntime.dll on windows) is loaded at runtime via dlopen by
// onnxruntime_go. Deployment ships the library alongside the binary; see
// third_party/onnxruntime/ for per-platform packages.

var (
	rtOnce sync.Once
	rtErr  error
)

// libraryPath resolves the ORT shared library:
//  1. $OPENAGENT_ORT_LIB when set
//  2. third_party/onnxruntime/<GOOS-GOARCH>/ relative to this package
//     (developer convenience)
//  3. the system library name — resolved via LD_LIBRARY_PATH / DYLD
//     paths / PATH
//
// The platform is picked EXPLICITLY (runtime.GOOS/GOARCH): probing every
// platform's file would stat darwin-arm64's dylib on an Intel Mac and
// hand back an architecture-mismatched library that dlopen rejects.
// Note: there is no third_party darwin-amd64 entry — onnxruntime 1.28.0
// and later ship no Intel-macOS build, so Intel Macs use the system
// library or OPENAGENT_ORT_LIB.
func libraryPath() string {
	if p := os.Getenv("OPENAGENT_ORT_LIB"); p != "" {
		return p
	}
	libName := "libonnxruntime.so"
	switch runtime.GOOS {
	case "darwin":
		libName = "libonnxruntime.dylib"
	case "windows":
		libName = "onnxruntime.dll"
	}
	_, file, _, ok := runtime.Caller(0)
	if ok {
		dir := filepath.Dir(file)
		c := filepath.Join(dir, "../../third_party/onnxruntime/"+runtime.GOOS+"-"+runtime.GOARCH, libName)
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return libName
}

// EnsureRuntime initializes the ONNX Runtime environment exactly once.
// Safe for concurrent use; the first error is sticky.
func EnsureRuntime() error {
	rtOnce.Do(func() {
		onnxruntime_go.SetSharedLibraryPath(libraryPath())
		rtErr = onnxruntime_go.InitializeEnvironment()
	})
	return rtErr
}

package wasmhost

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// Pack encodes (ptr, length) into a single u64 — high 32 bits = ptr, low 32 bits = len.
// This is the WASM ABI convention used by all plugin exports.
func Pack(ptr, length uint32) uint64 {
	return (uint64(ptr) << 32) | uint64(length)
}

// Unpack decodes a u64 into (ptr, length).
func Unpack(packed uint64) (ptr, length uint32) {
	return uint32(packed >> 32), uint32(packed & 0xFFFF_FFFF)
}

// ReadPacked reads bytes from guest memory at the (ptr, length) location
// encoded as a packed u64. Returns nil if the read fails or the pointer is
// zero.
func ReadPacked(mod api.Module, packed uint64) []byte {
	ptr, length := Unpack(packed)
	if ptr == 0 && length == 0 {
		return nil
	}
	data, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return nil
	}
	out := make([]byte, length)
	copy(out, data)
	return out
}

// ReadString reads N bytes from guest memory at ptr, returning a Go string.
func ReadString(mod api.Module, ptr, length uint32) string {
	if ptr == 0 && length == 0 {
		return ""
	}
	data, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return ""
	}
	return string(data)
}

// WriteString writes data into guest memory via the guest's alloc function.
// Caller provides the context for the alloc call.
func WriteString(ctx context.Context, mod api.Module, data []byte) uint64 {
	if len(data) == 0 {
		return 0
	}
	allocFn := mod.ExportedFunction("alloc")
	if allocFn == nil {
		return 0
	}
	results, err := allocFn.Call(ctx, uint64(len(data)))
	if err != nil || len(results) == 0 {
		return 0
	}
	ptr := uint32(results[0])
	mod.Memory().Write(ptr, data)
	return Pack(ptr, uint32(len(data)))
}

// CallWithInput calls a guest export that takes packed (ptr, len) input and
// returns a packed (ptr, len) result — the ABI convention shared by the CLI
// and agent plugin loaders (previously duplicated in both).
//
// Empty input calls the export with no arguments. A nil return means the
// export returned an empty result ((0, 0)); callers decide whether that is
// an error.
func CallWithInput(ctx context.Context, mod api.Module, fnName string, input []byte) ([]byte, error) {
	fn := mod.ExportedFunction(fnName)
	if fn == nil {
		return nil, fmt.Errorf("export %q not found", fnName)
	}

	var results []uint64
	if len(input) == 0 {
		var err error
		results, err = fn.Call(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", fnName, err)
		}
	} else {
		allocFn := mod.ExportedFunction("alloc")
		if allocFn == nil {
			return nil, fmt.Errorf("export %q: alloc not exported", fnName)
		}
		allocRes, err := allocFn.Call(ctx, uint64(len(input)))
		if err != nil || len(allocRes) == 0 {
			return nil, fmt.Errorf("%s: alloc: %w", fnName, err)
		}
		ptr := uint32(allocRes[0])
		if !mod.Memory().Write(ptr, input) {
			return nil, fmt.Errorf("%s: write out of bounds", fnName)
		}
		results, err = fn.Call(ctx, uint64(ptr), uint64(len(input)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", fnName, err)
		}
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("%s: no result", fnName)
	}
	return ReadPacked(mod, results[0]), nil
}

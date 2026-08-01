// Package bge implements an offline, single-binary Chinese/English text
// embedder based on BAAI/bge-small-zh-v1.5 (384-dim), running on
// ONNX Runtime.
//
// The model is embedded into the binary (go:embed), and the ONNX Runtime
// library is linked per platform from third_party/onnxruntime/<platform>/
// (static libonnxruntime.a where the toolchain allows; the Windows port
// uses the official DLL released at runtime). Deploying is a single
// binary — no external model service or embedding API.
//
// It implements openagent.Embedder and plugs into memory backends via
// sqlite.WithEmbedder for semantic knowledge recall.
package bge

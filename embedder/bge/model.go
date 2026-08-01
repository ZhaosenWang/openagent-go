package bge

import (
	"context"
	_ "embed"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yalue/onnxruntime_go"
)

// Model files embedded into the binary. The download script places them
// under third_party/models/bge-small-zh-v1.5/ and the build copies them
// into embed/ (see scripts/fetch-bge.sh). They are platform-independent.
//
//go:embed embed/model.onnx
var modelONNX []byte

//go:embed embed/vocab.txt
var vocabTXT []byte

// Dimension is the sentence-embedding dimension of bge-small-zh-v1.5:
// hidden_size=512, mean-pooled over the last_hidden_state output.
const Dimension = 512

// maxSeqLen caps token sequences at BERT's 512-token window (bge uses 512).
const maxSeqLen = 512

// queryInstruction is prepended to query texts per BGE's usage: retrieval
// queries are prefixed to separate query space from document space.
const queryInstruction = "为这个句子生成表示以用于检索相关文章："

// Embedder is an offline ONNX Runtime backed text embedder implementing
// openagent.Embedder. It is safe for concurrent use.
type Embedder struct {
	initOnce sync.Once
	initErr  error

	session   *onnxruntime_go.AdvancedSession
	tokenizer *wordpiece
	inIDs     *onnxruntime_go.Tensor[int64]
	inMask    *onnxruntime_go.Tensor[int64]
	inTypes   *onnxruntime_go.Tensor[int64]
	out       *onnxruntime_go.Tensor[float32]

	ready atomic.Bool
}

// New creates an Embedder, lazily initializing ONNX Runtime and loading
// the embedded model on first Embed call (so construction never fails
// for environments without the native library).
func New() *Embedder {
	return &Embedder{}
}

// init loads the ONNX session and tokenizer once.
func (e *Embedder) init() error {
	e.initOnce.Do(func() {
		if err := e.load(); err != nil {
			e.initErr = err
			return
		}
		e.ready.Store(true)
	})
	return e.initErr
}

func (e *Embedder) load() error {
	if err := EnsureRuntime(); err != nil {
		return err
	}

	// Tokenizer from embedded vocab.
	tok, err := newWordpieceFromBytes(vocabTXT)
	if err != nil {
		return fmt.Errorf("bge: tokenizer: %w", err)
	}
	e.tokenizer = tok

	inputs := []string{"input_ids", "attention_mask", "token_type_ids"}
	outputs := []string{"last_hidden_state"}

	shape := onnxruntime_go.Shape{1, maxSeqLen}
	e.inIDs, err = onnxruntime_go.NewTensor(shape, make([]int64, maxSeqLen))
	if err != nil {
		return fmt.Errorf("bge: input tensor: %w", err)
	}
	e.inMask, err = onnxruntime_go.NewTensor(shape, make([]int64, maxSeqLen))
	if err != nil {
		return fmt.Errorf("bge: mask tensor: %w", err)
	}
	e.inTypes, err = onnxruntime_go.NewTensor(shape, make([]int64, maxSeqLen))
	if err != nil {
		return fmt.Errorf("bge: type tensor: %w", err)
	}
	e.out, err = onnxruntime_go.NewTensor(onnxruntime_go.Shape{1, maxSeqLen, Dimension}, make([]float32, maxSeqLen*Dimension))
	if err != nil {
		return fmt.Errorf("bge: output tensor: %w", err)
	}

	// Load the model from embedded bytes (no temp file) with the input
	// tensors pre-bound; v1.x AdvancedSession.Run() takes no arguments.
	session, err := onnxruntime_go.NewAdvancedSessionWithONNXData(
		modelONNX, inputs, outputs,
		[]onnxruntime_go.Value{e.inIDs, e.inMask, e.inTypes},
		[]onnxruntime_go.Value{e.out}, nil)
	if err != nil {
		return fmt.Errorf("bge: onnx session: %w", err)
	}
	e.session = session

	// Warm-up run with zeros to finalize bindings.
	zeros := make([]int64, maxSeqLen)
	copy(e.inIDs.GetData(), zeros)
	copy(e.inMask.GetData(), zeros)
	copy(e.inTypes.GetData(), zeros)
	return e.session.Run()
}

// Embed implements openagent.Embedder: dense vector for a document
// (no query instruction prefix — BGE separates query/doc spaces).
func (e *Embedder) Embed(ctx context.Context, text string) ([]float64, error) {
	return e.embed(ctx, text)
}

// EmbedQuery embeds a retrieval query with BGE's query instruction
// prefix. Providers that distinguish query/doc should call this on the
// recall side (memory backends type-assert for it).
func (e *Embedder) EmbedQuery(ctx context.Context, text string) ([]float64, error) {
	return e.embed(ctx, queryInstruction+text)
}

// embed runs the shared inference path.
func (e *Embedder) embed(ctx context.Context, text string) ([]float64, error) {
	if err := e.init(); err != nil {
		return nil, err
	}

	ids := e.tokenizer.encode(text, maxSeqLen)
	if len(ids) == 0 {
		return nil, fmt.Errorf("bge: empty tokenization")
	}

	data := e.inIDs.GetData()
	for i := range data {
		if i < len(ids) {
			data[i] = int64(ids[i])
		} else {
			data[i] = 0
		}
	}
	mask := e.inMask.GetData()
	for i := range mask {
		if i < len(ids) {
			mask[i] = 1
		} else {
			mask[i] = 0
		}
	}
	types := e.inTypes.GetData()
	for i := range types {
		types[i] = 0
	}

	if err := e.session.Run(); err != nil {
		return nil, fmt.Errorf("bge: run: %w", err)
	}

	// last_hidden_state: [1, seq, dim]; mean-pool over non-padded tokens
	// (bge sentence-embedding convention), then normalize.
	out := e.out.GetData()
	if len(out) < Dimension {
		return nil, fmt.Errorf("bge: unexpected output length %d", len(out))
	}
	seq := len(out) / Dimension
	vec := make([]float64, Dimension)
	count := 0
	for t := 0; t < seq; t++ {
		if int(mask[t]) == 0 {
			continue
		}
		count++
		for d := 0; d < Dimension; d++ {
			vec[d] += float64(out[t*Dimension+d])
		}
	}
	if count == 0 {
		return nil, fmt.Errorf("bge: no unpadded tokens")
	}
	for d := range vec {
		vec[d] /= float64(count)
	}

	normalizeVec(vec)
	return vec, nil
}

// Dimensions implements openagent.Embedder.
func (e *Embedder) Dimensions() int { return Dimension }

// Close releases the ONNX session.
func (e *Embedder) Close() {
	if e.session != nil {
		e.session.Destroy()
		e.session = nil
	}
	if e.inIDs != nil {
		e.inIDs.Destroy()
		e.inMask.Destroy()
		e.inTypes.Destroy()
		e.out.Destroy()
	}
}

// newWordpieceFromBytes loads the vocab from embedded bytes.
func newWordpieceFromBytes(data []byte) (*wordpiece, error) {
	tmp := filepath.Join(os.TempDir(), "bge-vocab-"+randSuffix()+".txt")
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	return newWordpiece(tmp)
}

// normalizeVec makes the vector unit length (cosine retrieval).
func normalizeVec(v []float64) {
	var sum float64
	for _, x := range v {
		sum += x * x
	}
	if sum == 0 {
		return
	}
	inv := 1 / math.Sqrt(sum)
	for i := range v {
		v[i] *= inv
	}
}

func randSuffix() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}

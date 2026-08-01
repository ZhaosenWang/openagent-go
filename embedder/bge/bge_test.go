package bge

import (
	"context"
	"math"
	"testing"
)

// cosine computes cosine similarity between two vectors.
func cosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// TestEmbed_SemanticSimilarity verifies the embedding quality: semantically
// related Chinese sentences score higher than unrelated ones.
func TestEmbed_SemanticSimilarity(t *testing.T) {
	e := New()
	defer e.Close()
	ctx := context.Background()

	cases := []struct {
		name    string
		a, b, c string // a≈b should outrank a≈c
	}{
		{"terraform deploy vs infra deploy vs weather",
			"使用terraform部署基础设施",
			"用terraform做基础设施部署",
			"今天天气很好适合去公园散步"},
		{"docker vs kubernetes vs music",
			"用docker容器化部署应用",
			"把应用打包成docker镜像运行",
			"我喜欢听古典音乐"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			va, err := e.Embed(ctx, tc.a)
			if err != nil {
				t.Fatalf("Embed(%q): %v", tc.a, err)
			}
			vb, err := e.Embed(ctx, tc.b)
			if err != nil {
				t.Fatalf("Embed(%q): %v", tc.b, err)
			}
			vc, err := e.Embed(ctx, tc.c)
			if err != nil {
				t.Fatalf("Embed(%q): %v", tc.c, err)
			}

			ab := cosine(va, vb)
			ac := cosine(va, vc)
			t.Logf("sim(a,b)=%.4f sim(a,c)=%.4f", ab, ac)
			if len(va) != Dimension {
				t.Fatalf("dim = %d, want %d", len(va), Dimension)
			}
			if ab <= ac {
				t.Fatalf("related pair (%f) did not outrank unrelated (%f)", ab, ac)
			}
			if ab < 0.6 {
				t.Fatalf("related pair similarity too low: %f", ab)
			}
		})
	}
}

// TestEmbed_English verifies Latin text also tokenizes and embeds.
func TestEmbed_English(t *testing.T) {
	e := New()
	defer e.Close()
	v, err := e.Embed(context.Background(), "deploy infrastructure with terraform")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(v) != Dimension {
		t.Fatalf("dim = %d", len(v))
	}
}

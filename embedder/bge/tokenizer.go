package bge

import (
	"bufio"
	"os"
	"strings"
)

// wordpiece implements the BERT WordPiece tokenizer used by
// bge-small-zh-v1.5. Chinese text is split per character (wordpiece
// operates on characters for CJK); Latin text is tokenized by words with
// the standard ## continuation prefix.
//
// The model's vocab is a plain text file, one token per line.
type wordpiece struct {
	vocab map[string]int
}

// newWordpiece loads a vocab.txt (one token per line, index = token id).
func newWordpiece(vocabPath string) (*wordpiece, error) {
	f, err := os.Open(vocabPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	v := &wordpiece{vocab: make(map[string]int)}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	i := 0
	for sc.Scan() {
		v.vocab[sc.Text()] = i
		i++
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return v, nil
}

// encode tokenizes text into token ids, appending the [CLS] and [SEP]
// sentinel ids expected by BERT (bge: [CLS] text [SEP], max_length 512).
func (w *wordpiece) encode(text string, maxLen int) []int {
	cls := w.id("[CLS]")
	sep := w.id("[SEP]")
	if cls < 0 || sep < 0 {
		return nil
	}

	// Split into whitespace words (Chinese chars are kept as-is and
	// processed char-by-char below).
	ids := []int{cls}
	for _, word := range strings.Fields(normalize(text)) {
		for _, tok := range w.tokenizeWord(word) {
			if len(ids) >= maxLen-1 {
				break
			}
			if id, ok := w.vocab[tok]; ok {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) < maxLen {
		ids = append(ids, sep)
	}
	return ids
}

// tokenizeWord tokenizes a single word: CJK runs are split per
// character, others by longest-match WordPiece with ## continuations.
func (w *wordpiece) tokenizeWord(word string) []string {
	if isCJK(word) {
		// Chinese: one token per character (bge vocab covers all common
		// Han chars).
		out := make([]string, 0, len([]rune(word)))
		for _, r := range word {
			if _, ok := w.vocab[string(r)]; ok {
				out = append(out, string(r))
			}
		}
		return out
	}
	return w.wordpieceTokenize(word)
}

// wordpieceTokenize is the standard BERT WordPiece greedy longest-match.
func (w *wordpiece) wordpieceTokenize(word string) []string {
	word = strings.ToLower(word)
	var tokens []string
	start, end := 0, len(word)
	for start < len(word) {
		var curSubstr string
		for end > start {
			substr := word[start:end]
			if start > 0 {
				substr = "##" + substr
			}
			if _, ok := w.vocab[substr]; ok {
				curSubstr = substr
				break
			}
			end--
		}
		if curSubstr == "" {
			// Unknown token: emit [UNK] and skip one rune.
			tokens = append(tokens, "[UNK]")
			_, size := runeAt(word, start)
			start += size
			end = len(word)
			continue
		}
		tokens = append(tokens, curSubstr)
		// Strip the "##" continuation prefix from the matched length
		// (only present when start > 0). Without this, start==0 matches
		// would be decremented below zero and loop forever.
		matched := len(curSubstr)
		if start > 0 {
			matched -= 2
		}
		start += matched
		end = len(word)
	}
	return tokens
}

// id returns the vocab id of a token, or -1.
func (w *wordpiece) id(token string) int {
	if id, ok := w.vocab[token]; ok {
		return id
	}
	return -1
}

// normalize lowercases ASCII and trims surrounding spaces (BERT preprocessing).
func normalize(s string) string {
	return strings.TrimSpace(strings.ToLower(s))
}

// isCJK reports whether all runes of s are CJK ideographs (U+4E00–U+9FFF).
func isCJK(s string) bool {
	for _, r := range s {
		if r < 0x4E00 || r > 0x9FFF {
			return false
		}
	}
	return len(s) > 0
}

// runeAt returns the rune at byte offset start and its byte size.
func runeAt(s string, start int) (rune, int) {
	for i, r := range s {
		if i == start {
			return r, len(string(r))
		}
	}
	return 0, 1
}

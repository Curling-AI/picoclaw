// Package utils provides shared, reusable algorithms.
// This file implements a generic BM25 search engine.
//
// Usage:
//
//	type MyDoc struct { ID string; Body string }
//
//	corpus := []MyDoc{...}
//	engine := bm25.New(corpus, func(d MyDoc) string {
//	    return d.ID + " " + d.Body
//	})
//	results := engine.Search("my query", 5)
package utils

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// ── Tuning defaults ───────────────────────────────────────────────────────────

const (
	// DefaultBM25K1 is the term-frequency saturation factor (typical range 1.2–2.0).
	// Higher values give more weight to repeated terms.
	DefaultBM25K1 = 1.2

	// DefaultBM25B is the document-length normalization factor (0 = none, 1 = full).
	DefaultBM25B = 0.75
)

// BM25Engine is a BM25 search engine over a generic corpus.
// T is the document type; the caller supplies a TextFunc that extracts the
// searchable text from each document.
//
// The engine precomputes its index once at construction time and reuses it for
// subsequent searches. If the corpus content changes, construct a new engine.
type BM25Engine[T any] struct {
	corpus   []T
	textFunc func(T) string
	k1       float64
	b        float64
	index    *bm25Index
}

// BM25Option is a functional option to configure a BM25Engine.
type BM25Option func(*bm25Config)

type bm25Config struct {
	k1 float64
	b  float64
}

type bm25Index struct {
	entries    []bm25DocEntry
	idf        map[string]float32
	docLenNorm []float32
	posting    map[string][]int32
}

type bm25DocEntry struct {
	tf map[string]uint32
}

// WithK1 overrides the term-frequency saturation constant (default 1.2).
func WithK1(k1 float64) BM25Option {
	return func(c *bm25Config) { c.k1 = k1 }
}

// WithB overrides the document-length normalization factor (default 0.75).
func WithB(b float64) BM25Option {
	return func(c *bm25Config) { c.b = b }
}

// NewBM25Engine creates a BM25Engine for the given corpus.
//
//   - corpus   : slice of documents of any type T.
//   - textFunc : function that returns the searchable text for a document.
//   - opts     : optional tuning (WithK1, WithB).
//
// The corpus slice is referenced, not copied. Callers must not mutate it
// concurrently with Search().
func NewBM25Engine[T any](corpus []T, textFunc func(T) string, opts ...BM25Option) *BM25Engine[T] {
	cfg := bm25Config{k1: DefaultBM25K1, b: DefaultBM25B}
	for _, o := range opts {
		o(&cfg)
	}
	engine := &BM25Engine[T]{
		corpus:   corpus,
		textFunc: textFunc,
		k1:       cfg.k1,
		b:        cfg.b,
	}
	engine.index = buildBM25Index(corpus, textFunc, cfg.k1, cfg.b)
	return engine
}

// BM25Result is a single ranked result from a Search call.
type BM25Result[T any] struct {
	Document T
	Score    float32
}

// Search ranks the corpus against query and returns the top-k results.
// Returns an empty slice (not nil) when there are no matches.
//
// Complexity: O(|Q|×avgPostingLen + candidates × log k) per search after the
// one-time indexing work performed by NewBM25Engine.
func (e *BM25Engine[T]) Search(query string, topK int) []BM25Result[T] {
	if topK <= 0 {
		return []BM25Result[T]{}
	}

	queryTerms := bm25Tokenize(query)
	if len(queryTerms) == 0 {
		return []BM25Result[T]{}
	}

	if len(e.corpus) == 0 || e.index == nil {
		return []BM25Result[T]{}
	}

	// Step 4: score via posting lists
	// Deduplicate query terms to avoid double-weighting the same term.
	unique := bm25Dedupe(queryTerms)

	scores := make(map[int32]float32)
	for _, term := range unique {
		termIDF, ok := e.index.idf[term]
		if !ok {
			continue // term not in vocabulary → zero contribution
		}
		for _, docID := range e.index.posting[term] {
			freq := float32(e.index.entries[docID].tf[term])
			// TF_norm = freq * (k1+1) / (freq + docLenNorm)
			tfNorm := freq * float32(e.k1+1) / (freq + e.index.docLenNorm[docID])
			scores[docID] += termIDF * tfNorm
		}
	}

	if len(scores) == 0 {
		return []BM25Result[T]{}
	}

	// Step 5: top-K via fixed-size min-heap
	heap := make([]bm25ScoredDoc, 0, topK)

	for docID, sc := range scores {
		switch {
		case len(heap) < topK:
			heap = append(heap, bm25ScoredDoc{docID: docID, score: sc})
			if len(heap) == topK {
				bm25MinHeapify(heap)
			}
		case sc > heap[0].score:
			heap[0] = bm25ScoredDoc{docID: docID, score: sc}
			bm25SiftDown(heap, 0)
		}
	}

	sort.Slice(heap, func(i, j int) bool { return heap[i].score > heap[j].score })

	out := make([]BM25Result[T], len(heap))
	for i, h := range heap {
		out[i] = BM25Result[T]{
			Document: e.corpus[h.docID],
			Score:    h.score,
		}
	}
	return out
}

func buildBM25Index[T any](corpus []T, textFunc func(T) string, k1, b float64) *bm25Index {
	N := len(corpus)
	if N == 0 {
		return nil
	}

	entries := make([]bm25DocEntry, N)
	rawLens := make([]int, N)
	df := make(map[string]int, 64)
	totalLen := 0

	for i, doc := range corpus {
		tokens := bm25Tokenize(textFunc(doc))
		totalLen += len(tokens)
		rawLens[i] = len(tokens)

		tf := make(map[string]uint32, len(tokens))
		for _, t := range tokens {
			tf[t]++
		}
		for term := range tf {
			df[term]++
		}

		entries[i] = bm25DocEntry{tf: tf}
	}

	avgDocLen := float64(totalLen) / float64(N)
	if avgDocLen == 0 {
		avgDocLen = 1
	}

	idf := make(map[string]float32, len(df))
	for term, freq := range df {
		idf[term] = float32(math.Log(
			(float64(N)-float64(freq)+0.5)/(float64(freq)+0.5) + 1,
		))
	}

	docLenNorm := make([]float32, N)
	for i, rawLen := range rawLens {
		docLenNorm[i] = float32(k1 * (1 - b + b*float64(rawLen)/avgDocLen))
	}

	posting := make(map[string][]int32, len(df))
	for i, entry := range entries {
		for term := range entry.tf {
			posting[term] = append(posting[term], int32(i))
		}
	}

	return &bm25Index{
		entries:    entries,
		idf:        idf,
		docLenNorm: docLenNorm,
		posting:    posting,
	}
}

// bm25Tokenize splits s into lowercase tokens, stripping edge punctuation.
// Identifier-like tokens (snake_case, kebab-case, camelCase, dotted/slashed
// compounds) additionally emit their parts alongside the whole token — the
// same treatment Lucene's word_delimiter applies to code identifiers. Without
// this, a tool named "mcp_skip_skip_file_write" is a single opaque token that
// no natural query ("file write") can ever match, and the literal query
// "skip_file_write" misses it too (different compound). Both corpus and
// queries pass through here, so the expansion is symmetric.
func bm25Tokenize(s string) []string {
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields)*2)
	for _, raw := range fields {
		t := strings.Trim(raw, ".,;:!?\"'()/\\-_")
		if t == "" {
			continue
		}
		out = append(out, strings.ToLower(t))
		if parts := splitIdentifier(t); len(parts) > 1 {
			out = append(out, parts...)
		}
	}
	return out
}

// NormalizeIdentifier lowercases an identifier-like string and joins its
// parts with "_", collapsing separator and case variations: "skip_file_write",
// "skip-file-write" and "skipFileWrite" all normalize to "skip_file_write".
// Used for exact-name lookups over tool names.
func NormalizeIdentifier(s string) string {
	return strings.Join(splitIdentifier(s), "_")
}

// IdentifierParts returns the number of parts splitIdentifier finds in s —
// callers use it to tell compound identifiers ("file_write") apart from plain
// words ("file").
func IdentifierParts(s string) int {
	return len(splitIdentifier(s))
}

// splitIdentifier breaks an identifier-like token into lowercase parts on
// non-alphanumeric separators and lower→upper camelCase boundaries. Unicode
// letters count as word characters, so accented words ("calendário") stay
// whole. Returns the lowercased parts; a plain word yields a single part.
func splitIdentifier(token string) []string {
	var parts []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			parts = append(parts, strings.ToLower(b.String()))
			b.Reset()
		}
	}
	var prev rune
	for _, r := range token {
		switch {
		case unicode.IsUpper(r):
			if unicode.IsLower(prev) || unicode.IsDigit(prev) {
				flush() // camelCase boundary
			}
			b.WriteRune(r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			flush()
		}
		prev = r
	}
	flush()
	return parts
}

// bm25Dedupe returns a new slice with duplicate tokens removed,
// preserving first-occurrence order.
func bm25Dedupe(tokens []string) []string {
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	return out
}

type bm25ScoredDoc struct {
	docID int32
	score float32
}

// bm25MinHeapify builds a min-heap in-place using Floyd's algorithm: O(k).
func bm25MinHeapify(h []bm25ScoredDoc) {
	for i := len(h)/2 - 1; i >= 0; i-- {
		bm25SiftDown(h, i)
	}
}

// bm25SiftDown restores the min-heap property starting at node i: O(log k).
func bm25SiftDown(h []bm25ScoredDoc, i int) {
	n := len(h)
	for {
		smallest := i
		l, r := 2*i+1, 2*i+2
		if l < n && h[l].score < h[smallest].score {
			smallest = l
		}
		if r < n && h[r].score < h[smallest].score {
			smallest = r
		}
		if smallest == i {
			break
		}
		h[i], h[smallest] = h[smallest], h[i]
		i = smallest
	}
}

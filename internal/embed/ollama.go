// Package embed wraps a local Ollama server for text embeddings.
//
// The model is configurable (default qwen3-embedding:0.6b). Embedding dimension is whatever
// the model returns — nothing here assumes a fixed size, so swapping models
// only requires a reindex. A future reranker (e.g. bge-reranker-v2-m3) would
// slot in as a separate stage after retrieval, behind the search.Reranker
// interface in internal/search.
//
// Network note: this talks to OLLAMA_HOST, which defaults to localhost but is a
// plain env var. If it is set to a remote server, every document chunk and every
// search query is sent there.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultModel is the embedder we pull by default. qwen3-embedding:0.6b beat
// bge-m3 on a labeled eval of this corpus (better top-1 / MRR at the same 1024
// dims and query latency) — see docs/embedding-models.md.
const DefaultModel = "qwen3-embedding:0.6b"

// Asymmetric-retrieval conditioning differs by model family.
//
// bge/nomic: a static prefix on each side ("search_document: " / "search_query: ").
// Qwen3-Embedding: documents are embedded bare, and the *query* carries a
// natural-language task instruction ("Instruct: <task>\nQuery: <text>"). Using
// the bge prefixes on Qwen3 measurably hurts it, so we pick the recipe per model.
const (
	docPrefix   = "search_document: "
	queryPrefix = "search_query: "
	// qwenTask is the retrieval instruction prepended to Qwen3 queries.
	qwenTask = "Given a search query, retrieve relevant Claude Code / Codex session transcripts."
)

// isQwen3 reports whether the model uses Qwen3-Embedding's instruction recipe.
func isQwen3(model string) bool {
	return strings.Contains(strings.ToLower(model), "qwen3")
}

// docInput conditions a document for embedding per the model's recipe.
func (c *Client) docInput(text string) string {
	if isQwen3(c.Model) {
		return text // Qwen3 embeds documents without a prefix
	}
	return docPrefix + text
}

// queryInput conditions a search query for embedding per the model's recipe.
func (c *Client) queryInput(q string) string {
	if isQwen3(c.Model) {
		return "Instruct: " + qwenTask + "\nQuery: " + q
	}
	return queryPrefix + q
}

type Client struct {
	http  *http.Client
	url   string
	Model string
}

// New returns a client. Model resolution order: explicit arg, RECALL_EMBED_MODEL, DefaultModel.
// Host resolution: OLLAMA_HOST or http://localhost:11434.
func New(model string) *Client {
	if model == "" {
		model = os.Getenv("RECALL_EMBED_MODEL")
	}
	if model == "" {
		model = DefaultModel
	}
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://localhost:11434"
	}
	return &Client{
		http:  &http.Client{Timeout: 5 * time.Minute},
		url:   host,
		Model: model,
	}
}

type embedReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResp struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Host is the Ollama server this client embeds with.
func (c *Client) Host() string { return c.url }

func (c *Client) post(ctx context.Context, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(embedReq{Model: c.Model, Input: texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("ollama unreachable at %s (is it running?): %w", c.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama /api/embed %s: %s", resp.Status, b)
	}
	var er embedResp
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, err
	}
	return er.Embeddings, nil
}

// EmbedDocuments embeds chunk texts (with the document prefix). Cancelling ctx
// abandons the request in flight.
func (c *Client) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	in := make([]string, len(texts))
	for i, t := range texts {
		in[i] = c.docInput(t)
	}
	vs, err := c.post(ctx, in)
	if err != nil {
		return nil, err
	}
	if len(vs) != len(texts) {
		return nil, fmt.Errorf("embed count mismatch: got %d want %d", len(vs), len(texts))
	}
	return vs, nil
}

// EmbedQuery embeds a search query (with the query prefix).
func (c *Client) EmbedQuery(q string) ([]float32, error) {
	vs, err := c.post(context.Background(), []string{c.queryInput(q)})
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, fmt.Errorf("no embedding returned for query")
	}
	return vs[0], nil
}

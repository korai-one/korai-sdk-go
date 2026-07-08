package korai

import (
	"context"
)

// RetrievedChunk is a single passage returned by a retrieval query.
// Mirrors the Python SDK's RetrievedChunk with idiomatic Go field
// names.
type RetrievedChunk struct {
	ChunkID      string         `json:"chunk_id"`
	Text         string         `json:"text"`
	Score        float64        `json:"score"`
	SourceID     string         `json:"source_id"`
	Citation     string         `json:"citation,omitempty"`
	Language     string         `json:"language,omitempty"`
	Jurisdiction string         `json:"jurisdiction,omitempty"`
	URL          string         `json:"url,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// RetrieveOptions narrows down a Retrieve call.
type RetrieveOptions struct {
	Corpus       string
	Languages    []string // "fr", "de", "it", "en"
	Jurisdiction []string
	TopK         int
	MinScore     float64
}

// Retrieve runs a hybrid (vector + BM25) lookup on a Korai-indexed
// corpus.
//
// TODO(cloud): The orchestrator does not yet expose a public RAG
// endpoint — retrieval is currently coupled to the
// vertical-fiduciaire backend. This method returns
// ErrNotImplemented until the planned /v1/rag/retrieve route ships.
func (c *Client) Retrieve(ctx context.Context, query string, opts RetrieveOptions) ([]RetrievedChunk, error) {
	return nil, ErrNotImplemented
}

// Rerank applies a cross-encoder rerank pass over a candidate set.
//
// TODO(cloud): pending /v1/rag/rerank.
func (c *Client) Rerank(ctx context.Context, query string, chunks []RetrievedChunk, topN int) ([]RetrievedChunk, error) {
	return nil, ErrNotImplemented
}

// RetrieveAndRerank is a convenience wrapping Retrieve+Rerank.
//
// TODO(cloud): depends on the same endpoints as Retrieve / Rerank.
func (c *Client) RetrieveAndRerank(ctx context.Context, query string, opts RetrieveOptions, topN int) ([]RetrievedChunk, error) {
	return nil, ErrNotImplemented
}

// Embed embeds a batch of texts using Korai Cloud's default
// embedding model (bge-m3). Returns one vector per input.
//
// TODO(cloud): pending /v1/embeddings.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	return nil, ErrNotImplemented
}

// IndexDocuments uploads a batch of documents to a private corpus.
//
// TODO(cloud): pending /v1/rag/index.
func (c *Client) IndexDocuments(ctx context.Context, corpus string, documents []map[string]any) (int, error) {
	return 0, ErrNotImplemented
}

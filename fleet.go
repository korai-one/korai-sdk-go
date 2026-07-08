package korai

import "context"

// Fleet catalog types. Where ListModels reports models a connected
// worker currently advertises (live availability), the fleet manifest is
// the catalog: every model the network is configured to run — with
// variants, VRAM tiers, and backends — independent of who is connected.
// Backed by the public GET /v1/fleet/{manifest,stats} endpoints.

// FleetVariant is one downloadable variant of a model (quant/backend).
type FleetVariant struct {
	HFRepo     string `json:"hf_repo,omitempty"`
	HFFilename string `json:"hf_filename,omitempty"`
	SizeGB     int    `json:"size_gb,omitempty"`
	MinVRAMGB  int    `json:"min_vram_gb,omitempty"`
	Backend    string `json:"backend,omitempty"`
}

// FleetModel is a model in the catalog, with its variants and roles.
type FleetModel struct {
	Creator      string                  `json:"creator,omitempty"`
	License      string                  `json:"license,omitempty"`
	Family       string                  `json:"family,omitempty"`
	TotalParams  string                  `json:"total_params,omitempty"`
	ActiveParams string                  `json:"active_params,omitempty"`
	ContextLen   int                     `json:"context_len,omitempty"`
	Multimodal   bool                    `json:"multimodal,omitempty"`
	Roles        []string                `json:"roles,omitempty"`
	Variants     map[string]FleetVariant `json:"variants,omitempty"`
}

// FleetManifest is the full model catalog.
type FleetManifest struct {
	SchemaVersion int                   `json:"schema_version"`
	Version       int64                 `json:"version"`
	PublishedAt   string                `json:"published_at,omitempty"`
	Models        map[string]FleetModel `json:"models,omitempty"`
	Tiers         map[string]any        `json:"tiers,omitempty"`
	APITiers      map[string]any        `json:"api_tiers,omitempty"`
	Deprecated    []string              `json:"deprecated,omitempty"`
}

// FleetStats is the summary-counts view of the manifest.
type FleetStats struct {
	SchemaVersion int    `json:"schema_version"`
	Version       int64  `json:"version"`
	PublishedAt   string `json:"published_at,omitempty"`
	Models        int    `json:"models"`
	Tiers         int    `json:"tiers"`
	APITiers      int    `json:"api_tiers"`
	Deprecated    int    `json:"deprecated"`
}

// GetFleetManifest returns the full fleet manifest — the catalog of
// models the network can serve, with variants, VRAM tiers, and backends.
func (c *Client) GetFleetManifest(ctx context.Context) (*FleetManifest, error) {
	httpResp, err := c.gen.GetFleetManifest(ctx)
	var out FleetManifest
	if err := c.genDecode(httpResp, err, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetFleetStats returns summary counts (number of models, tiers, etc.).
func (c *Client) GetFleetStats(ctx context.Context) (*FleetStats, error) {
	httpResp, err := c.gen.GetFleetStats(ctx)
	var out FleetStats
	if err := c.genDecode(httpResp, err, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

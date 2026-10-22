// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"gitea.dev/modules/log"
)

// ModelCatalogURL is the shared model catalog. It is an open database (MIT) that
// describes models across providers, which matters because most provider APIs expose
// only an id: the catalog is the only source of input modalities for them.
var ModelCatalogURL = "https://models.dev/api.json"

const (
	catalogTTL       = 12 * time.Hour
	catalogRetryWait = 5 * time.Minute
)

// catalogLookupWait bounds how long an answer may wait for the catalog; a model whose
// modalities never arrive is treated as able to see, so the question still gets answered.
const catalogLookupWait = 10 * time.Second

// AcceptsImages reports whether a model may be shown pictures. A model the catalog
// describes as text-only never receives them; a model it does not know about does,
// because a brand new vision model is worth more than a rejected attachment. When the
// catalog is unavailable entirely, pictures go through too.
func AcceptsImages(ctx context.Context, providerType, modelID string) bool {
	if modelID == "" {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, catalogLookupWait)
	defer cancel()
	catalog := LoadCatalog(ctx)
	if catalog == nil {
		return true
	}
	return slices.Contains(catalog.InputModalities(providerType, modelID), "image")
}

// catalogModel is what the catalog reports about one model.
type catalogModel struct {
	InputModalities []string
	// OutputLimit is the largest answer the model accepts, in tokens; 0 when unknown.
	OutputLimit int
}

// Catalog holds what the shared catalog reports, narrowed to the providers this instance
// knows about.
type Catalog struct {
	providers map[string]map[string]catalogModel // catalog provider id -> model id -> info
}

// InputModalities returns the input modalities of a model, or nil when unknown.
func (c *Catalog) InputModalities(providerType, modelID string) []string {
	return c.model(providerType, modelID).InputModalities
}

// OutputLimit returns the largest answer the model accepts, in tokens, or 0 when unknown.
func (c *Catalog) OutputLimit(providerType, modelID string) int {
	return c.model(providerType, modelID).OutputLimit
}

func (c *Catalog) model(providerType, modelID string) catalogModel {
	if c == nil || modelID == "" {
		return catalogModel{}
	}
	info, ok := Provider(providerType)
	if !ok || info.ModelsDevProvider == "" {
		return catalogModel{}
	}
	return c.providers[info.ModelsDevProvider][modelID]
}

// MaxOutputTokens returns the largest answer the model accepts, in tokens, or 0 when the
// catalog does not say. A caller decides what an unknown model gets.
func MaxOutputTokens(ctx context.Context, providerType, modelID string) int {
	if modelID == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, catalogLookupWait)
	defer cancel()
	return LoadCatalog(ctx).OutputLimit(providerType, modelID)
}

// DefaultMaxTokens is what a request carries when the catalog says nothing: room for an
// answer and the reasoning ahead of it.
const DefaultMaxTokens = 4096

var (
	catalogMu      sync.Mutex
	catalogCache   *Catalog
	catalogFetched time.Time
	catalogNextTry time.Time
)

// resetCatalogCache drops the cached copy; used by tests.
func resetCatalogCache() {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	catalogCache, catalogFetched, catalogNextTry = nil, time.Time{}, time.Time{}
}

// LoadCatalog returns the shared catalog, fetching it when the cache is stale.
// It never blocks a request for long: on failure it returns nil and retries later.
func LoadCatalog(ctx context.Context) *Catalog {
	catalogMu.Lock()
	defer catalogMu.Unlock()

	now := time.Now()
	if catalogCache != nil && now.Sub(catalogFetched) < catalogTTL {
		return catalogCache
	}
	if catalogCache == nil && now.Before(catalogNextTry) {
		return nil
	}

	fetched, err := fetchCatalog(ctx)
	catalogFetched = now
	if err != nil {
		catalogNextTry = now.Add(catalogRetryWait)
		if catalogCache == nil {
			log.Warn("model catalog %s is unavailable, continuing without model metadata: %v", ModelCatalogURL, err)
			return nil
		}
		log.Warn("model catalog refresh failed, keeping the previous copy: %v", err)
		return catalogCache
	}
	catalogCache = fetched
	return catalogCache
}

// catalogPayload is the subset of the catalog this instance reads.
type catalogPayload struct {
	Models map[string]struct {
		Modalities struct {
			Input []string `json:"input"`
		} `json:"modalities"`
		Limit struct {
			Output int `json:"output"`
		} `json:"limit"`
	} `json:"models"`
}

func fetchCatalog(ctx context.Context) (*Catalog, error) {
	wanted := map[string]bool{}
	for _, info := range Providers() {
		if info.ModelsDevProvider != "" {
			wanted[info.ModelsDevProvider] = true
		}
	}
	if len(wanted) == 0 {
		return &Catalog{providers: map[string]map[string]catalogModel{}}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelCatalogURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("model catalog returned status %d", resp.StatusCode)
	}

	// decode only the providers in use; the full document covers hundreds of them. The
	// streaming walk is what keeps this cheap: the document is far too big to decode whole.
	catalog := &Catalog{providers: map[string]map[string]catalogModel{}}
	dec := json.NewDecoder(resp.Body)
	if _, err := dec.Token(); err != nil { // opening brace
		return nil, err
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		providerID, _ := key.(string)
		if !wanted[providerID] {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
			continue
		}
		var payload catalogPayload
		if err := dec.Decode(&payload); err != nil {
			return nil, err
		}
		models := make(map[string]catalogModel, len(payload.Models))
		for id, m := range payload.Models {
			models[id] = catalogModel{
				InputModalities: normalizeModalities(m.Modalities.Input),
				OutputLimit:     m.Limit.Output,
			}
		}
		catalog.providers[providerID] = models
	}
	return catalog, nil
}

// normalizeModalities trims, de-duplicates and sorts modality names.
func normalizeModalities(modalities []string) []string {
	if len(modalities) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(modalities))
	for _, modality := range modalities {
		if modality == "" || seen[modality] {
			continue
		}
		seen[modality] = true
		out = append(out, modality)
	}
	slices.Sort(out)
	return out
}

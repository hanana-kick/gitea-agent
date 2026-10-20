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

// Catalog holds the input modalities the shared catalog reports, narrowed to the
// providers this instance knows about.
type Catalog struct {
	providers map[string]map[string][]string // catalog provider id -> model id -> input modalities
}

// InputModalities returns the input modalities of a model, or nil when unknown.
func (c *Catalog) InputModalities(providerType, modelID string) []string {
	if c == nil || modelID == "" {
		return nil
	}
	info, ok := Provider(providerType)
	if !ok || info.ModelsDevProvider == "" {
		return nil
	}
	return c.providers[info.ModelsDevProvider][modelID]
}

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
		return &Catalog{providers: map[string]map[string][]string{}}, nil
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

	// decode only the providers in use; the full document covers hundreds of them
	catalog := &Catalog{providers: map[string]map[string][]string{}}
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
		models := make(map[string][]string, len(payload.Models))
		for id, m := range payload.Models {
			models[id] = normalizeModalities(m.Modalities.Input)
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

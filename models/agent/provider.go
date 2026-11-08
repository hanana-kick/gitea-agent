// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"slices"
	"strings"

	"gitea.dev/models/db"
	"gitea.dev/modules/json"
	"gitea.dev/modules/timeutil"

	"xorm.io/builder"
)

// ProviderModel is one selectable model of a provider.
type ProviderModel struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Inputs lists the input modalities the provider reported, when it reports them.
	Inputs []string `json:"inputs,omitempty"`
}

// DisplayName returns the name to show, falling back to the identifier.
func (m ProviderModel) DisplayName() string {
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}

// Provider represents a user-configured LLM provider (BYOK).
// Secrets are encrypted at rest using setting.SecretKey.
type Provider struct {
	ID int64 `xorm:"pk autoincr"`
	// UserID records who created the provider. Providers are instance-wide settings,
	// so it is never used to scope reads or writes.
	UserID   int64  `xorm:"INDEX NOT NULL"`
	Name     string `xorm:"NOT NULL"` // display name chosen by user
	Provider string `xorm:"NOT NULL"` // openai, openrouter, nvidia, opencode
	BaseURL  string // endpoint in use; resolved from the provider type when unset
	// Model is the first of Models, kept as a plain column for API clients and tests.
	Model      string `xorm:"NOT NULL"`
	ModelsJSON string `xorm:"TEXT"` // selected models as a JSON array
	// APIKeyID points at the credential in agent_api_key; 0 means none is bound.
	APIKeyID    int64              `xorm:"INDEX NOT NULL DEFAULT 0"`
	HeadersJSON string             `xorm:"TEXT"` // optional extra request headers as a JSON object
	HasKey      bool               `xorm:"-"`    // computed: a key is bound
	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (Provider) TableName() string {
	return "agent_provider"
}

// Models returns the selected models; an empty selection falls back to Model.
func (p *Provider) Models() []ProviderModel {
	if p.ModelsJSON == "" {
		if p.Model == "" {
			return nil
		}
		return []ProviderModel{{ID: p.Model}}
	}
	models := []ProviderModel{}
	if err := json.Unmarshal([]byte(p.ModelsJSON), &models); err != nil {
		return nil
	}
	return models
}

// SetModels stores the selected models; the first one also becomes the plain Model column.
func (p *Provider) SetModels(models []ProviderModel) error {
	if len(models) == 0 {
		p.ModelsJSON = ""
		p.Model = ""
		return nil
	}
	data, err := json.Marshal(models)
	if err != nil {
		return err
	}
	p.ModelsJSON = string(data)
	p.Model = models[0].ID
	return nil
}

// ModelIDs returns the selected model identifiers.
func (p *Provider) ModelIDs() []string {
	models := p.Models()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

// CustomHeaders returns the provider's extra request headers, or nil when unset.
func (p *Provider) CustomHeaders() map[string]string {
	if p.HeadersJSON == "" {
		return nil
	}
	headers := map[string]string{}
	if err := json.Unmarshal([]byte(p.HeadersJSON), &headers); err != nil {
		return nil
	}
	return headers
}

// SetCustomHeaders stores the extra request headers; an empty map is stored as NULL.
func (p *Provider) SetCustomHeaders(headers map[string]string) error {
	if len(headers) == 0 {
		p.HeadersJSON = ""
		return nil
	}
	data, err := json.Marshal(headers)
	if err != nil {
		return err
	}
	p.HeadersJSON = string(data)
	return nil
}

// HeaderNames returns the configured header names, sorted, for display (values are secret).
func (p *Provider) HeaderNames() []string {
	headers := p.CustomHeaders()
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// NormalizeModels trims identifiers, drops blanks and removes duplicates while keeping order.
func NormalizeModels(models []ProviderModel) []ProviderModel {
	seen := map[string]bool{}
	out := make([]ProviderModel, 0, len(models))
	for _, m := range models {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, ProviderModel{ID: id, Name: strings.TrimSpace(m.Name), Inputs: normalizeModalities(m.Inputs)})
	}
	return out
}

// normalizeModalities trims, de-duplicates and sorts modality names.
func normalizeModalities(inputs []string) []string {
	if len(inputs) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(inputs))
	for _, input := range inputs {
		input = strings.TrimSpace(input)
		if input == "" || seen[input] {
			continue
		}
		seen[input] = true
		out = append(out, input)
	}
	slices.Sort(out)
	return out
}

// FindProvidersOptions describes filtering options for finding providers.
type FindProvidersOptions struct {
	db.ListOptions
	Provider string // filter by provider type, empty means all
	Keyword  string // case-insensitive match on the name or the model
}

func (opts FindProvidersOptions) ToConds() builder.Cond {
	cond := builder.NewCond()
	if opts.Provider != "" {
		cond = cond.And(builder.Eq{"provider": opts.Provider})
	}
	if opts.Keyword != "" {
		cond = cond.And(builder.Or(
			db.BuildCaseInsensitiveLike("name", opts.Keyword),
			db.BuildCaseInsensitiveLike("model", opts.Keyword),
		))
	}
	return cond
}

func (opts FindProvidersOptions) ToOrders() string {
	return "name"
}

// ErrProviderNotFound is returned when a provider is not found.
type ErrProviderNotFound struct {
	ID int64
}

func (err ErrProviderNotFound) Error() string {
	return "provider not found"
}

// Is reports whether target is an ErrProviderNotFound regardless of the ID.
func (err ErrProviderNotFound) Is(target error) bool {
	_, ok := target.(ErrProviderNotFound)
	return ok
}

func init() {
	db.RegisterModel(new(Provider))
}

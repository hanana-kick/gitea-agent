// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"net/http"

	agent_service "gitea.dev/services/agent"
	"gitea.dev/services/context"
)

// APIKeyResponse describes a stored credential. The secret itself is never part of a
// response: it can only be replaced, through the web UI.
type APIKeyResponse struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name"`
	UsedBy   []string `json:"used_by"`
	Created  int64    `json:"created"`
	LastUsed int64    `json:"last_used"`
}

// ListAPIKeys lists the credentials an operator can bind providers to.
func ListAPIKeys(ctx *context.APIContext) {
	keys, err := agent_service.ListAPIKeys(ctx)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	usage, err := agent_service.APIKeyUsage(ctx)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	response := make([]APIKeyResponse, 0, len(keys))
	for _, key := range keys {
		usedBy := usage[key.ID]
		if usedBy == nil {
			usedBy = []string{}
		}
		response = append(response, APIKeyResponse{
			ID:       key.ID,
			Name:     key.Name,
			UsedBy:   usedBy,
			Created:  int64(key.CreatedUnix),
			LastUsed: int64(key.LastUsedUnix),
		})
	}
	ctx.JSON(http.StatusOK, response)
}

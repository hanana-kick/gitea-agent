// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package swagger

import (
	api "gitea.dev/modules/structs"
)

// AgentSession
// swagger:response AgentSession
type swaggerResponseAgentSession struct {
	// in:body
	Body api.AgentSession `json:"body"`
}

// AgentSessionList
// swagger:response AgentSessionList
type swaggerResponseAgentSessionList struct {
	// in:body
	Body []api.AgentSession `json:"body"`
}

// AgentRun
// swagger:response AgentRun
type swaggerResponseAgentRun struct {
	// in:body
	Body api.AgentRun `json:"body"`
}

// AgentMessageList
// swagger:response AgentMessageList
type swaggerResponseAgentMessageList struct {
	// in:body
	Body struct {
		List    []api.AgentMessage `json:"list"`
		HasMore bool               `json:"has_more"`
	} `json:"body"`
}

// AgentApprovalList
// swagger:response AgentApprovalList
type swaggerResponseAgentApprovalList struct {
	// in:body
	Body struct {
		List  []api.AgentApproval `json:"list"`
		Total int64               `json:"total"`
	} `json:"body"`
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
)

// RenameAgentReviewRoleToDecision keeps the model an instance assigned to the review role: the
// role is the one that decides, and it is the same job under a name that says so.
func RenameAgentReviewRoleToDecision(ctx context.Context, x base.EngineMigration) error {
	_, err := x.Exec("UPDATE agent_model_role SET role = ? WHERE role = ?", "decision", "review")
	return err
}

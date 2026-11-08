// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
)

// agentToolApprovalRunColumns are the columns of a run that the previous migration added to the
// approval table: its frozen shape named the wrong table, so the table grew fields no approval
// has. Nothing reads them.
var agentToolApprovalRunColumns = []string{
	"user_id", "model", "error", "reply_repo_id", "reply_issue_id", "status_comment_id",
	"started_unix", "finished_unix",
}

// DropAgentToolApprovalRunColumns takes back the columns of a run that the approval table was
// given by mistake. A fresh installation never had them, and this then does nothing.
func DropAgentToolApprovalRunColumns(ctx context.Context, x base.EngineMigration) error {
	sess := x.NewSession()
	defer sess.Close()
	if err := sess.Begin(); err != nil {
		return err
	}

	present := make([]string, 0, len(agentToolApprovalRunColumns))
	for _, column := range agentToolApprovalRunColumns {
		exist, err := x.Dialect().IsColumnExist(sess.DB(), ctx, "agent_tool_approval", column)
		if err != nil {
			return err
		}
		if exist {
			present = append(present, column)
		}
	}
	if len(present) == 0 {
		return sess.Commit() // a fresh installation never had them
	}
	if err := base.DropTableColumns(sess, "agent_tool_approval", present...); err != nil {
		return err
	}
	return sess.Commit()
}

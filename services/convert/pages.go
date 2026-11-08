// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package convert

import (
	"context"

	actions_model "gitea.dev/models/actions"
	repo_model "gitea.dev/models/repo"
	api "gitea.dev/modules/structs"
)

// ToPagesDeployment converts a deployed pages record with the run that deployed it, if any.
func ToPagesDeployment(run *actions_model.ActionRun, deployment *repo_model.PagesDeployment) *api.PagesDeployment {
	converted := &api.PagesDeployment{
		ID:        deployment.ID,
		Source:    deployment.Source,
		CommitSHA: deployment.CommitSHA,
		RunID:     deployment.RunID,
		Status:    deployment.Status,
		Error:     deployment.Error,
		CreatedAt: deployment.CreatedUnix.AsTime(),
	}
	if run != nil {
		converted.RunIndex = run.Index
		converted.URL = run.Link()
	}
	return converted
}

// ToPages converts how a repository publishes its pages, with the deployment that is served.
func ToPages(ctx context.Context, repo *repo_model.Repository, pages *repo_model.Pages, deployment *repo_model.PagesDeployment) *api.Pages {
	converted := &api.Pages{
		URL:          repo.PagesURL(),
		Source:       pages.Source,
		Branch:       pages.Branch,
		Dir:          pages.Dir,
		CustomDomain: pages.CustomDomain,
	}
	if deployment == nil {
		return converted
	}
	converted.CommitSHA = deployment.CommitSHA
	var run *actions_model.ActionRun
	if deployment.RunID > 0 {
		// a run that is gone leaves the deployment without its link, which is not a failure
		if run, _ = actions_model.GetRunByRepoAndID(ctx, repo.ID, deployment.RunID); run != nil {
			run.Repo = repo
		}
	}
	converted.Deployment = ToPagesDeployment(run, deployment)
	return converted
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import (
	"net/http"
	"path"
	"strings"

	actions_model "gitea.dev/models/actions"
	repo_model "gitea.dev/models/repo"
	unit_model "gitea.dev/models/unit"
	"gitea.dev/modules/log"
	"gitea.dev/modules/templates"
	"gitea.dev/modules/web"
	"gitea.dev/services/context"
	"gitea.dev/services/forms"
	repo_service "gitea.dev/services/repository"
)

const tplPages templates.TplName = "repo/settings/pages"

// PagesSettings shows how the repository publishes its pages and what it published.
func PagesSettings(ctx *context.Context) {
	pages, has, err := repo_model.GetPages(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.ServerError("GetPages", err)
		return
	}
	deployments, err := repo_model.ListPagesDeployments(ctx, ctx.Repo.Repository.ID, repo_service.PagesDeploymentLimit)
	if err != nil {
		ctx.ServerError("ListPagesDeployments", err)
		return
	}
	artifacts, err := repo_service.FindPagesArtifacts(ctx, ctx.Repo.Repository.ID, repo_service.PagesDeploymentLimit)
	if err != nil {
		ctx.ServerError("FindPagesArtifacts", err)
		return
	}

	// the page says "not published" by asking whether there is a setting, so it is passed as nil
	// when there is none, and a live deployment of a page that was unpublished is not reported
	var published *repo_model.Pages
	if has {
		published = pages
	}

	runIDs := make([]int64, 0, len(deployments)+len(artifacts))
	for _, deployment := range deployments {
		if deployment.RunID > 0 {
			runIDs = append(runIDs, deployment.RunID)
		}
	}
	for _, artifact := range artifacts {
		runIDs = append(runIDs, artifact.RunID)
	}
	runs, err := actions_model.GetRunsByRepoAndID(ctx, ctx.Repo.Repository.ID, runIDs)
	if err != nil {
		ctx.ServerError("GetRunsByRepoAndID", err)
		return
	}
	runsByID := make(map[int64]*actions_model.ActionRun, len(runs))
	for _, run := range runs {
		runsByID[run.ID] = run
	}

	ctx.Data["Title"] = ctx.Tr("repo.settings.pages")
	ctx.Data["PageIsSettingsPages"] = true
	ctx.Data["Pages"] = published
	ctx.Data["PagesURL"] = ctx.Repo.Repository.PagesURL()
	ctx.Data["PagesDirs"] = repo_service.PagesDirs
	ctx.Data["PagesArtifactName"] = repo_service.PagesArtifactName
	ctx.Data["PagesDeployments"] = deployments
	ctx.Data["PagesRuns"] = runsByID
	ctx.Data["PagesArtifacts"] = artifacts
	ctx.Data["PagesLiveCommit"], ctx.Data["PagesLiveDeployment"] = pagesLiveState(ctx, published, deployments)
	ctx.Data["PagesDeployable"] = ctx.Repo.Permission.CanWrite(unit_model.TypeCode) && !ctx.Repo.Repository.IsArchived
	ctx.HTML(http.StatusOK, tplPages)
}

// pagesLiveState is what a reader is being served right now, if anything: the commit of the
// branch the pages are read from, or the deployment that was built.
func pagesLiveState(ctx *context.Context, pages *repo_model.Pages, deployments []*repo_model.PagesDeployment) (string, *repo_model.PagesDeployment) {
	if pages == nil {
		return "", nil
	}
	if pages.Source == repo_model.PagesSourceBranch {
		if ctx.Repo.GitRepo == nil {
			return "", nil
		}
		commit, err := ctx.Repo.GitRepo.GetBranchCommit(ctx, pages.Branch)
		if err != nil {
			return "", nil
		}
		entry, err := commit.GetTreeEntryByPath(ctx, ctx.Repo.GitRepo, path.Join(strings.Trim(pages.Dir, "/"), repo_model.PagesIndex))
		if err != nil || entry == nil || entry.IsDir() {
			return "", nil
		}
		return commit.ID.String(), nil
	}
	for _, deployment := range deployments {
		if deployment.Status == repo_model.PagesStatusBuilt {
			return deployment.CommitSHA, deployment
		}
	}
	return "", nil
}

// PagesSettingsPost stores how the repository publishes its pages.
func PagesSettingsPost(ctx *context.Context) {
	form := web.GetForm[*forms.PagesForm](ctx)
	pages, err := repo_service.SetPages(ctx, ctx.Repo.Repository, repo_service.PagesOptions{
		Source:       form.Source,
		Branch:       form.Branch,
		Dir:          form.Dir,
		CustomDomain: form.CustomDomain,
	})
	if err != nil {
		ctx.Flash.Error(err.Error())
	} else {
		ctx.Flash.Success(ctx.Tr("repo.settings.pages_saved"))
		log.Info("Repository %s pages: published from %s", ctx.Repo.Repository.FullName(), pages.Source)
	}
	ctx.Redirect(ctx.Repo.RepoLink + "/settings/pages")
}

// PagesDeploy publishes the pages of the repository from an artifact of a workflow run.
func PagesDeploy(ctx *context.Context) {
	repo := ctx.Repo.Repository
	form := web.GetForm[*forms.DeployPagesForm](ctx)

	pages, has, err := repo_model.GetPages(ctx, repo.ID)
	if err != nil {
		ctx.ServerError("GetPages", err)
		return
	}
	if !has || pages.Source != repo_model.PagesSourceActions {
		ctx.Flash.Error(ctx.Tr("repo.settings.pages_deploy_actions_only"))
		ctx.Redirect(ctx.Repo.RepoLink + "/settings/pages")
		return
	}
	artifact, err := repo_service.GetPagesArtifact(ctx, repo.ID, form.ArtifactID)
	if err != nil {
		ctx.Flash.Error(err.Error())
		ctx.Redirect(ctx.Repo.RepoLink + "/settings/pages")
		return
	}
	var run *actions_model.ActionRun
	if artifact.RunID > 0 {
		if run, err = actions_model.GetRunByRepoAndID(ctx, repo.ID, artifact.RunID); err != nil {
			ctx.ServerError("GetRunByRepoAndID", err)
			return
		}
	}
	if _, err := repo_service.DeployPagesFromArtifact(ctx, repo, run, artifact, ctx.Doer.ID); err != nil {
		ctx.Flash.Error(err.Error())
	} else {
		ctx.Flash.Success(ctx.Tr("repo.settings.pages_deployed"))
		log.Info("Repository %s pages: deployed artifact %d", repo.FullName(), artifact.ID)
	}
	ctx.Redirect(ctx.Repo.RepoLink + "/settings/pages")
}

// PagesUnpublish removes what the repository published.
func PagesUnpublish(ctx *context.Context) {
	if err := repo_service.RemovePages(ctx, ctx.Repo.Repository); err != nil {
		ctx.ServerError("RemovePages", err)
		return
	}
	ctx.Flash.Success(ctx.Tr("repo.settings.pages_unpublished"))
	log.Info("Repository %s pages: unpublished", ctx.Repo.Repository.FullName())
	ctx.Redirect(ctx.Repo.RepoLink + "/settings/pages")
}

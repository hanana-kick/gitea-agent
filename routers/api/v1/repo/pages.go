// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"net/http"

	actions_model "gitea.dev/models/actions"
	repo_model "gitea.dev/models/repo"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/web"
	"gitea.dev/services/context"
	"gitea.dev/services/convert"
	repo_service "gitea.dev/services/repository"
)

// GetPages returns how a repository publishes its pages
func GetPages(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/pages repository repoGetPages
	// ---
	// summary: Get a repository's pages
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   in: path
	//   description: owner of the repo
	//   type: string
	//   required: true
	// - name: repo
	//   in: path
	//   description: name of the repo
	//   type: string
	//   required: true
	// responses:
	//   "200":
	//     "$ref": "#/responses/Pages"
	//   "404":
	//     "$ref": "#/responses/notFound"

	pages, has, err := repo_model.GetPages(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	if !has {
		ctx.APIErrorNotFound()
		return
	}
	deployment, _, err := repo_model.GetLatestPagesDeployment(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	ctx.JSON(http.StatusOK, convert.ToPages(ctx, ctx.Repo.Repository, pages, deployment))
}

// UpdatePages stores how a repository publishes its pages
func UpdatePages(ctx *context.APIContext) {
	// swagger:operation PUT /repos/{owner}/{repo}/pages repository repoUpdatePages
	// ---
	// summary: Update how a repository publishes its pages
	// consumes:
	// - application/json
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   in: path
	//   description: owner of the repo
	//   type: string
	//   required: true
	// - name: repo
	//   in: path
	//   description: name of the repo
	//   type: string
	//   required: true
	// - name: body
	//   in: body
	//   schema:
	//     "$ref": "#/definitions/UpdatePagesOption"
	// responses:
	//   "200":
	//     "$ref": "#/responses/Pages"
	//   "403":
	//     "$ref": "#/responses/forbidden"

	form := web.GetForm[*api.UpdatePagesOption](ctx)
	pages, err := repo_service.SetPages(ctx, ctx.Repo.Repository, repo_service.PagesOptions{
		Source:       form.Source,
		Branch:       form.Branch,
		Dir:          form.Dir,
		CustomDomain: form.CustomDomain,
	})
	if err != nil {
		ctx.APIError(http.StatusBadRequest, err.Error())
		return
	}
	deployment, has, err := repo_model.GetLatestPagesDeployment(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	if !has {
		deployment = nil
	}
	ctx.JSON(http.StatusOK, convert.ToPages(ctx, ctx.Repo.Repository, pages, deployment))
}

// DeployPages publishes the pages of a repository from an artifact of a workflow run
func DeployPages(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/pages/deploy repository repoDeployPages
	// ---
	// summary: Deploy a repository's pages from an artifact of a workflow run
	// consumes:
	// - application/json
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   in: path
	//   description: owner of the repo
	//   type: string
	//   required: true
	// - name: repo
	//   in: path
	//   description: name of the repo
	//   type: string
	//   required: true
	// - name: body
	//   in: body
	//   schema:
	//     "$ref": "#/definitions/DeployPagesOption"
	// responses:
	//   "201":
	//     "$ref": "#/responses/PagesDeployment"
	//   "403":
	//     "$ref": "#/responses/forbidden"
	//   "404":
	//     "$ref": "#/responses/notFound"

	form := web.GetForm[*api.DeployPagesOption](ctx)
	repo := ctx.Repo.Repository

	pages, has, err := repo_model.GetPages(ctx, repo.ID)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	if !has || pages.Source != repo_model.PagesSourceActions {
		ctx.APIError(http.StatusForbidden, "this repository does not publish its pages from a workflow")
		return
	}
	artifact, err := repo_service.GetPagesArtifact(ctx, repo.ID, form.ArtifactID)
	if err != nil {
		ctx.APIErrorNotFound(err.Error())
		return
	}
	var run *actions_model.ActionRun
	if artifact.RunID > 0 {
		if run, err = actions_model.GetRunByRepoAndID(ctx, repo.ID, artifact.RunID); err != nil {
			ctx.APIErrorInternal(err)
			return
		}
		run.Repo = repo
	}
	deployment, err := repo_service.DeployPagesFromArtifact(ctx, repo, run, artifact, ctx.Doer.ID)
	if err != nil {
		ctx.APIError(http.StatusBadRequest, err.Error())
		return
	}
	ctx.JSON(http.StatusCreated, convert.ToPagesDeployment(run, deployment))
}

// DeletePages removes what a repository published
func DeletePages(ctx *context.APIContext) {
	// swagger:operation DELETE /repos/{owner}/{repo}/pages repository repoDeletePages
	// ---
	// summary: Unpublish a repository's pages
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   in: path
	//   description: owner of the repo
	//   type: string
	//   required: true
	// - name: repo
	//   in: path
	//   description: name of the repo
	//   type: string
	//   required: true
	// responses:
	//   "204":
	//     "$ref": "#/responses/empty"
	//   "404":
	//     "$ref": "#/responses/notFound"

	if err := repo_service.RemovePages(ctx, ctx.Repo.Repository); err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	ctx.Status(http.StatusNoContent)
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/httpcache"
	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
	repo_service "gitea.dev/services/repository"
)

// pagesPath works out which file of the pages a request means, as a path inside the published
// root. A path that climbs out of it, or names something a repository cannot hold, is refused
// rather than resolved.
func pagesPath(raw string) (string, bool) {
	cleaned := path.Clean("/" + strings.ReplaceAll(raw, "\\", "/"))
	if cleaned == "/" || strings.HasSuffix(raw, "/") {
		cleaned = path.Join(cleaned, repo_model.PagesIndex)
	}
	relative := strings.TrimPrefix(cleaned, "/")
	if relative == "" || relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
		return "", false
	}
	return relative, true
}

// maxPageFileSize is the largest file a published page may consist of: a page is read whole so
// that it can be served with range requests, and pages are not big.
const maxPageFileSize = 8 * 1024 * 1024

// Pages serves the pages a repository publishes: what a workflow deployed, or the files
// committed under the configured directory of the configured branch.
func Pages(ctx *context.Context) {
	repo := ctx.Repo.Repository
	pages, has, err := repo_model.GetPages(ctx, repo.ID)
	if err != nil {
		ctx.ServerError("GetPages", err)
		return
	}
	if !has {
		ctx.NotFound(nil)
		return
	}
	file, ok := pagesPath(ctx.PathParam("*"))
	if !ok {
		ctx.NotFound(nil)
		return
	}
	switch pages.Source {
	case repo_model.PagesSourceBranch:
		servePagesFromBranch(ctx, pages, file)
	case repo_model.PagesSourceActions:
		servePagesFromActions(ctx, repo.ID, file)
	default:
		ctx.NotFound(nil)
	}
}

// servePagesFromBranch serves what is committed under the configured directory of the branch the
// repository publishes from. Nothing is built: a page is read as it was committed.
func servePagesFromBranch(ctx *context.Context, pages *repo_model.Pages, file string) {
	gitRepo := ctx.Repo.GitRepo
	if gitRepo == nil {
		ctx.NotFound(nil)
		return
	}
	commit, err := gitRepo.GetBranchCommit(ctx, pages.Branch)
	if err != nil {
		ctx.NotFound(err)
		return
	}
	entry, err := commit.GetTreeEntryByPath(ctx, gitRepo, pageTreePath(pages, file))
	if err != nil || entry == nil {
		ctx.NotFound(err)
		return
	}
	if entry.IsDir() || entry.IsSubModule() {
		// a directory is served by its index, the way a web server does it
		file = path.Join(file, repo_model.PagesIndex)
		if entry, err = commit.GetTreeEntryByPath(ctx, gitRepo, pageTreePath(pages, file)); err != nil || entry == nil {
			ctx.NotFound(err)
			return
		}
	}
	lastCommit, err := gitRepo.GetTreePathLatestCommit(ctx, commit.ID.String(), pageTreePath(pages, file))
	if err != nil {
		ctx.ServerError("GetTreePathLatestCommit", err)
		return
	}
	blob := entry.Blob(gitRepo)
	lastModified := &lastCommit.Committer.When
	if httpcache.HandleGenericETagPrivateCache(ctx.Req, ctx.Resp, `"`+blob.ID.String()+`"`, lastModified) {
		return
	}
	if blob.Size(ctx) > maxPageFileSize {
		ctx.HTTPError(http.StatusRequestEntityTooLarge, "a page file is smaller than this")
		return
	}
	dataRc, err := blob.DataAsync(ctx)
	if err != nil {
		ctx.ServerError("DataAsync", err)
		return
	}
	defer dataRc.Close()
	content, err := io.ReadAll(dataRc)
	if err != nil {
		ctx.ServerError("ReadAll", err)
		return
	}
	writePage(ctx, path.Base(file), content, *lastModified)
}

// pageTreePath is where a file of a page lives in the tree of its branch.
func pageTreePath(pages *repo_model.Pages, file string) string {
	return path.Join(strings.Trim(pages.Dir, "/"), file)
}

// servePagesFromActions serves the content of the last deployment of the repository.
func servePagesFromActions(ctx *context.Context, repoID int64, file string) {
	deployment, has, err := repo_model.GetLatestPagesDeployment(ctx, repoID)
	if err != nil {
		ctx.ServerError("GetLatestPagesDeployment", err)
		return
	}
	if !has {
		ctx.NotFound(nil)
		return
	}
	root := repo_service.PagesBuildDir(repoID)
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(file)))
	if err != nil {
		ctx.NotFound(err)
		return
	}
	if info.IsDir() {
		file = path.Join(file, repo_model.PagesIndex)
		info, err = os.Stat(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			ctx.NotFound(err)
			return
		}
	}
	// the deployment is part of what a file is: two deployments can hold the same bytes under the
	// same name, and a reader of the older one is not reading the newer page
	etag := `"` + strconv.FormatInt(deployment.ID, 10) + "-" + strconv.FormatInt(info.ModTime().UnixNano(), 10) + `"`
	lastModified := info.ModTime()
	if httpcache.HandleGenericETagPrivateCache(ctx.Req, ctx.Resp, etag, &lastModified) {
		return
	}
	if info.Size() > maxPageFileSize {
		ctx.HTTPError(http.StatusRequestEntityTooLarge, "a page file is smaller than this")
		return
	}
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
	if err != nil {
		ctx.ServerError("ReadFile", err)
		return
	}
	writePage(ctx, path.Base(file), content, info.ModTime())
}

// writePage answers with one file of a published page. A page is something a reader wrote and is
// served from the instance's own address: it gets the type its name asks for, and an origin of
// its own so that the session it is read with is nothing it can reach.
func writePage(ctx *context.Context, name string, content []byte, lastModified time.Time) {
	ctx.Resp.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-popups allow-modals")
	ctx.Resp.Header().Set("X-Content-Type-Options", "nosniff")
	ctx.Resp.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(setting.StaticCacheTime.Seconds())))
	http.ServeContent(ctx.Resp, ctx.Req, name, lastModified, bytes.NewReader(content))
}

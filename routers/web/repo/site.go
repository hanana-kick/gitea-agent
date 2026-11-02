// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"bytes"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"gitea.dev/modules/git"
	"gitea.dev/modules/httpcache"
	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
)

// pagesPath works out which file of the page a request means. A path that climbs out of the page
// directory, or names something a repository cannot hold, is refused rather than resolved.
func pagesPath(raw string) (string, bool) {
	cleaned := path.Clean("/" + strings.ReplaceAll(raw, "\\", "/"))
	if cleaned == "/" {
		cleaned = "/" + context.RepoPagesIndex
	} else if strings.HasSuffix(raw, "/") {
		cleaned = path.Join(cleaned, context.RepoPagesIndex)
	}
	relative := strings.TrimPrefix(cleaned, "/")
	if relative == "" || relative == "." || strings.HasPrefix(relative, "../") {
		return "", false
	}
	return path.Join(context.RepoPagesDir, relative), true
}

// maxPageFileSize is the largest file a published page may consist of: a page is read whole so
// that it can be served with range requests, and pages are not big.
const maxPageFileSize = 8 * 1024 * 1024

// Pages serves the page a repository publishes: the files under public/ on the branch it is read
// at. What a reader gets is what was committed and released, which is the point of publishing a
// page from a repository at all.
func Pages(ctx *context.Context) {
	file, ok := pagesPath(ctx.PathParam("*"))
	if !ok {
		ctx.NotFound(nil)
		return
	}
	gitRepo := ctx.Repo.GitRepo
	if gitRepo == nil {
		ctx.NotFound(nil)
		return
	}
	// a page is published from the branch the repository is meant for, not from a request's ref
	commit, err := gitRepo.GetBranchCommit(ctx, ctx.Repo.Repository.DefaultBranch)
	if err != nil {
		ctx.NotFound(err)
		return
	}
	entry, err := commit.GetTreeEntryByPath(ctx, gitRepo, file)
	if err != nil {
		if git.IsErrNotExist(err) {
			ctx.NotFound(err)
			return
		}
		ctx.ServerError("GetTreeEntryByPath", err)
		return
	}
	if entry.IsDir() || entry.IsSubModule() {
		// a directory is served by its index, the way a web server does it
		file = path.Join(file, context.RepoPagesIndex)
		if entry, err = commit.GetTreeEntryByPath(ctx, gitRepo, file); err != nil {
			ctx.NotFound(err)
			return
		}
	}
	lastCommit, err := gitRepo.GetTreePathLatestCommit(ctx, commit.ID.String(), file)
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

	// a published page is something a reader wrote, served from the instance's own address: it
	// gets the type its name asks for, and an origin of its own so that the session it is read
	// with is nothing it can reach
	ctx.Resp.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-popups allow-modals")
	ctx.Resp.Header().Set("X-Content-Type-Options", "nosniff")
	ctx.Resp.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(setting.StaticCacheTime.Seconds())))
	http.ServeContent(ctx.Resp, ctx.Req, path.Base(file), *lastModified, bytes.NewReader(content))
}

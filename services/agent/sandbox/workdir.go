// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/git/gitrepo"
	"gitea.dev/modules/setting"
)

// Workdir is a checkout of one revision, on disk, ready to be mounted.
type Workdir struct {
	Dir string
	// Ref is the revision that was checked out, which is what a caller says it worked on.
	Ref string
}

// Close takes the checkout away: nothing a command did to it outlives the call.
func (w *Workdir) Close() {
	if w != nil && w.Dir != "" {
		_ = os.RemoveAll(w.Dir)
	}
}

// Prepare lays out the revision of a repository in a directory of the instance's own. The objects
// are shared with the repository rather than copied, so a large repository costs one checkout
// rather than two.
func Prepare(ctx context.Context, repo *repo_model.Repository, ref string) (*Workdir, error) {
	if ref = refOf(repo, ref); ref == "" {
		return nil, fmt.Errorf("the repository %s has no revision to work on", repo.FullName())
	}
	root, err := os.MkdirTemp(setting.AppDataPath, "agent-command-")
	if err != nil {
		return nil, fmt.Errorf("make a place for the checkout: %w", err)
	}
	dir := filepath.Join(root, "repo")
	if err := git.Clone(ctx, gitrepo.RepoLocalPath(repo), dir, git.CloneRepoOptions{
		Shared: true, NoCheckout: true, Quiet: true,
	}); err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("lay out %s: %w", repo.FullName(), err)
	}
	checkout := gitcmd.NewCommand("checkout", "--force").AddDynamicArguments(ref).WithDir(dir)
	if _, _, err := checkout.RunStdString(ctx); err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("check out %s of %s: %w", ref, repo.FullName(), err)
	}
	return &Workdir{Dir: dir, Ref: ref}, nil
}

// refOf is the revision a command works on: what the caller named, or the repository's default.
func refOf(repo *repo_model.Repository, ref string) string {
	if ref != "" {
		return ref
	}
	return repo.DefaultBranch
}

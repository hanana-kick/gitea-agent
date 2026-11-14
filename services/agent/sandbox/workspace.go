// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/git/gitrepo"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
)

// Work is a checkout of one revision that the commands of a run share: a run that builds something
// and then tests it finds what it built, and a commit it made is what the next command reads. It
// lives under the instance's own data, and is taken away when it has not been used for a while.
type Work struct {
	Dir string
	Ref string
	// repoPath and ref name the checkout in the list the instance keeps of them.
	repoPath string

	mu sync.Mutex
}

// key names a checkout by the repository and the revision it holds.
func workKey(repo *repo_model.Repository, ref string) string {
	return gitrepo.RepoLocalPath(repo) + "\x00" + ref
}

var (
	workMu sync.Mutex
	work   = map[string]*Work{}
)

// Workspace returns the checkout a command runs in, laying it out the first time and reusing it
// after that. It is brought to the revision asked for before every use, so a commit the run made
// is in it whatever ran in between.
func Workspace(ctx context.Context, repo *repo_model.Repository, ref string) (*Work, error) {
	ref = refOf(repo, ref)
	if ref == "" {
		return nil, fmt.Errorf("the repository %s has no revision to work on", repo.FullName())
	}
	key := workKey(repo, ref)

	workMu.Lock()
	found, ok := work[key]
	workMu.Unlock()
	if !ok {
		prepared, err := layOut(ctx, repo, ref)
		if err != nil {
			return nil, err
		}
		workMu.Lock()
		// another caller may have laid out the same revision in the meantime
		if existing, raced := work[key]; raced {
			_ = os.RemoveAll(filepath.Dir(prepared.Dir))
			found = existing
		} else {
			work[key] = prepared
			found = prepared
		}
		workMu.Unlock()
	}

	found.mu.Lock()
	defer found.mu.Unlock()
	if err := found.syncFrom(ctx, repo); err != nil {
		return nil, err
	}
	return found, nil
}

// layOut makes the checkout of one revision. The objects are shared with the repository rather
// than copied, so a large repository costs one working tree rather than two.
func layOut(ctx context.Context, repo *repo_model.Repository, ref string) (*Work, error) {
	root, err := os.MkdirTemp(setting.AppDataPath, "agent-workspace-")
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
	return &Work{Dir: dir, Ref: ref, repoPath: gitrepo.RepoLocalPath(repo)}, nil
}

// syncFrom brings the checkout to what the repository holds now: a run that commits a change is
// read back by the next command, which is what makes building on it worth anything.
func (w *Work) syncFrom(ctx context.Context, repo *repo_model.Repository) error {
	fetch := gitcmd.NewCommand("fetch", "--quiet", "--force", "origin").AddDynamicArguments(w.Ref).WithDir(w.Dir)
	if _, _, err := fetch.RunStdString(ctx); err != nil {
		return fmt.Errorf("read %s from %s: %w", w.Ref, repo.FullName(), err)
	}
	// what the run itself left in the working tree is kept: only the revision moves
	checkout := gitcmd.NewCommand("checkout", "--force", "FETCH_HEAD").WithDir(w.Dir)
	if _, _, err := checkout.RunStdString(ctx); err != nil {
		return fmt.Errorf("check out %s of %s: %w", w.Ref, repo.FullName(), err)
	}
	return nil
}

// Forgets drops the checkouts of a repository, for one that has been deleted.
func Forget(repo *repo_model.Repository) {
	path := gitrepo.RepoLocalPath(repo)
	workMu.Lock()
	defer workMu.Unlock()
	for key, found := range work {
		if !strings.HasPrefix(key, path+"\x00") {
			continue
		}
		_ = os.RemoveAll(filepath.Dir(found.Dir))
		delete(work, key)
	}
}

// Sweep takes away the checkouts that have not been used for a day: a run that is over leaves its
// working tree behind, and the instance is not a place to keep them.
func Sweep() {
	entries, err := os.ReadDir(setting.AppDataPath)
	if err != nil {
		return
	}
	deadline := time.Now().Add(-24 * time.Hour)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "agent-workspace-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(deadline) {
			continue
		}
		path := filepath.Join(setting.AppDataPath, entry.Name())
		workMu.Lock()
		used := false
		for _, found := range work {
			if strings.HasPrefix(found.Dir, path) {
				used = true
				break
			}
		}
		workMu.Unlock()
		if used {
			continue
		}
		if err := os.RemoveAll(path); err == nil {
			log.Debug("Agent sandbox: took away the checkout %s", path)
		}
	}
}

// refOf is the revision a command works on: what the caller named, or the repository's default.
func refOf(repo *repo_model.Repository, ref string) string {
	if strings.TrimSpace(ref) != "" {
		return strings.TrimSpace(ref)
	}
	return repo.DefaultBranch
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repository

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	actions_model "gitea.dev/models/actions"
	"gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/storage"
)

// The artifact a workflow uploads to publish pages, the way a forge names it.
const PagesArtifactName = "pages"

// PagesDeploymentLimit is how many deployments the history of a repository keeps.
const PagesDeploymentLimit = 20

// ErrPagesArtifactInvalid is returned when an artifact can not be published as pages.
type ErrPagesArtifactInvalid struct{ Reason string }

func (err ErrPagesArtifactInvalid) Error() string { return err.Reason }

// Is reports whether target is an ErrPagesArtifactInvalid.
func (err ErrPagesArtifactInvalid) Is(target error) bool {
	_, ok := target.(ErrPagesArtifactInvalid)
	return ok
}

// PagesOptions are the settings a repository's pages are published with.
type PagesOptions struct {
	Source       string
	Branch       string
	Dir          string
	CustomDomain string
}

// PagesDirs are the directories a branch-sourced page may be read from: the repository root and
// the folders a forge offers, plus the public folder this instance published from before pages
// could be configured.
var PagesDirs = []string{"/", "/docs", "/public"}

// normalizePagesOptions validates what a settings form asked for.
func normalizePagesOptions(repo *repo_model.Repository, opts PagesOptions) (PagesOptions, error) {
	if !repo_model.IsValidPagesSource(opts.Source) {
		return opts, ErrPagesArtifactInvalid{Reason: "a source is required"}
	}
	opts.Branch = strings.TrimSpace(opts.Branch)
	opts.Dir = strings.Trim(strings.TrimSpace(opts.Dir), "/")
	if opts.Dir != "" {
		opts.Dir = "/" + opts.Dir
	} else {
		opts.Dir = "/"
	}
	if opts.Source == repo_model.PagesSourceBranch {
		if opts.Branch == "" {
			opts.Branch = repo.DefaultBranch
		}
		validDir := false
		for _, dir := range PagesDirs {
			validDir = validDir || dir == opts.Dir
		}
		if !validDir {
			return opts, ErrPagesArtifactInvalid{Reason: opts.Dir + " is not a directory a page is published from"}
		}
	} else {
		opts.Branch, opts.Dir = "", ""
	}
	opts.CustomDomain = strings.ToLower(strings.TrimSpace(opts.CustomDomain))
	return opts, nil
}

// SetPages stores how a repository publishes pages, creating the setting on first save.
func SetPages(ctx context.Context, repo *repo_model.Repository, opts PagesOptions) (*repo_model.Pages, error) {
	opts, err := normalizePagesOptions(repo, opts)
	if err != nil {
		return nil, err
	}
	pages, has, err := repo_model.GetPages(ctx, repo.ID)
	if err != nil {
		return nil, err
	}
	if !has {
		pages = &repo_model.Pages{RepoID: repo.ID}
	}
	pages.Source, pages.Branch, pages.Dir, pages.CustomDomain = opts.Source, opts.Branch, opts.Dir, opts.CustomDomain
	if pages.ID == 0 {
		if err := db.Insert(ctx, pages); err != nil {
			return nil, fmt.Errorf("insert pages of repository %d: %w", repo.ID, err)
		}
		return pages, nil
	}
	if _, err := db.GetEngine(ctx).ID(pages.ID).Cols("source", "branch", "dir", "custom_domain", "updated_unix").Update(pages); err != nil {
		return nil, fmt.Errorf("update pages of repository %d: %w", repo.ID, err)
	}
	return pages, nil
}

// RemovePages unpublishes the pages of a repository: the setting and what was deployed go, the
// history stays as the record of what was published.
func RemovePages(ctx context.Context, repo *repo_model.Repository) error {
	if _, err := db.GetEngine(ctx).Where("repo_id = ?", repo.ID).Delete(&repo_model.Pages{}); err != nil {
		return fmt.Errorf("delete pages of repository %d: %w", repo.ID, err)
	}
	if err := os.RemoveAll(PagesBuildDir(repo.ID)); err != nil {
		log.Error("remove built pages of repository %d: %v", repo.ID, err)
	}
	return nil
}

// PagesBuildDir is where the content of an actions deployment is unpacked.
func PagesBuildDir(repoID int64) string {
	return filepath.Join(setting.AppDataPath, "pages", strconv.FormatInt(repoID, 10))
}

// FindPagesArtifacts returns the artifacts of a repository a workflow published as pages, newest
// first, with the run each belongs to.
func FindPagesArtifacts(ctx context.Context, repoID int64, limit int) ([]*actions_model.ActionArtifact, error) {
	artifacts, err := actions_model.FindReadableArtifacts(ctx, actions_model.FindArtifactsOptions{
		ListOptions:  db.ListOptions{PageSize: limit},
		RepoID:       repoID,
		ArtifactName: PagesArtifactName,
	})
	if err != nil {
		return nil, fmt.Errorf("find pages artifacts of repository %d: %w", repoID, err)
	}
	return artifacts, nil
}

// GetPagesArtifact returns one artifact of a repository that holds its pages.
func GetPagesArtifact(ctx context.Context, repoID, artifactID int64) (*actions_model.ActionArtifact, error) {
	artifact := &actions_model.ActionArtifact{}
	has, err := db.GetEngine(ctx).Where("id = ?", artifactID).And("repo_id = ?", repoID).Get(artifact)
	if err != nil {
		return nil, fmt.Errorf("get artifact %d of repository %d: %w", artifactID, repoID, err)
	}
	if !has {
		return nil, ErrPagesArtifactInvalid{Reason: "no such artifact in this repository"}
	}
	return artifact, nil
}

// DeployPagesFromArtifact unpacks the artifact as the pages of the repository, replacing what is
// served. The repository has to publish from actions for this to be its content.
func DeployPagesFromArtifact(ctx context.Context, repo *repo_model.Repository, run *actions_model.ActionRun, artifact *actions_model.ActionArtifact, deployerID int64) (*repo_model.PagesDeployment, error) {
	if artifact.RepoID != repo.ID || artifact.ArtifactName != PagesArtifactName {
		return nil, ErrPagesArtifactInvalid{Reason: "the artifact does not hold the pages of this repository"}
	}
	if artifact.Status != actions_model.ArtifactStatusUploadConfirmed {
		return nil, ErrPagesArtifactInvalid{Reason: "the artifact is not fully uploaded yet"}
	}

	// the new content is unpacked beside the old one and swapped in, so a reader never sees a
	// half-written page and a failed deployment leaves the previous one served
	build := PagesBuildDir(repo.ID)
	if err := os.MkdirAll(filepath.Dir(build), 0o755); err != nil {
		return nil, fmt.Errorf("create the pages directory: %w", err)
	}
	staging, err := os.MkdirTemp(filepath.Dir(build), "pages-staging-")
	if err != nil {
		return nil, fmt.Errorf("create staging directory for pages: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(staging); err != nil {
			log.Error("remove pages staging directory %q: %v", staging, err)
		}
	}()

	if err := unpackPagesArtifact(artifact, staging); err != nil {
		return deployPagesFailure(ctx, repo, run, artifact, deployerID, err)
	}
	if !hasPagesIndex(staging) {
		return deployPagesFailure(ctx, repo, run, artifact, deployerID,
			ErrPagesArtifactInvalid{Reason: "the artifact holds no " + repo_model.PagesIndex})
	}

	if err := os.RemoveAll(build); err != nil {
		return nil, fmt.Errorf("remove previous pages of repository %d: %w", repo.ID, err)
	}
	if err := os.Rename(staging, build); err != nil {
		return nil, fmt.Errorf("move new pages into place: %w", err)
	}

	deployment := &repo_model.PagesDeployment{
		RepoID: repo.ID, Source: repo_model.PagesSourceActions, Status: repo_model.PagesStatusBuilt,
		CommitSHA: artifact.CommitSHA, DeployerID: deployerID,
	}
	if run != nil {
		deployment.RunID, deployment.CommitSHA = run.ID, run.CommitSHA
	}
	deployment.ArtifactID = artifact.ID
	if err := db.Insert(ctx, deployment); err != nil {
		return nil, fmt.Errorf("record pages deployment of repository %d: %w", repo.ID, err)
	}
	prunePagesDeployments(ctx, repo.ID)
	return deployment, nil
}

// deployPagesFailure records a deployment that did not happen, so the settings page says why
// instead of showing an empty history.
func deployPagesFailure(ctx context.Context, repo *repo_model.Repository, run *actions_model.ActionRun, artifact *actions_model.ActionArtifact, deployerID int64, cause error) (*repo_model.PagesDeployment, error) {
	deployment := &repo_model.PagesDeployment{
		RepoID: repo.ID, Source: repo_model.PagesSourceActions, Status: repo_model.PagesStatusFailed,
		Error: cause.Error(), CommitSHA: artifact.CommitSHA, ArtifactID: artifact.ID, DeployerID: deployerID,
	}
	if run != nil {
		deployment.RunID, deployment.CommitSHA = run.ID, run.CommitSHA
	}
	if err := db.Insert(ctx, deployment); err != nil {
		return nil, fmt.Errorf("record failed pages deployment of repository %d: %w", repo.ID, err)
	}
	return nil, cause
}

// unpackPagesArtifact writes the files of an artifact into dir. Only regular files are written:
// an archive can name a path outside the directory or a link to somewhere else, and a page that
// is served from the instance's own address is not the place to find out what that does.
func unpackPagesArtifact(artifact *actions_model.ActionArtifact, dir string) error {
	fd, err := storage.ActionsArtifacts.Open(artifact.StoragePath)
	if err != nil {
		return fmt.Errorf("open artifact %d: %w", artifact.ID, err)
	}
	defer fd.Close()

	// the size is known, so the archive is read from memory rather than from a file on disk
	if artifact.FileSize > maxPagesArtifactSize {
		return ErrPagesArtifactInvalid{Reason: "the artifact is larger than a page may be"}
	}
	content, err := io.ReadAll(io.LimitReader(fd, maxPagesArtifactSize+1))
	if err != nil {
		return fmt.Errorf("read artifact %d: %w", artifact.ID, err)
	}
	if int64(len(content)) > maxPagesArtifactSize {
		return ErrPagesArtifactInvalid{Reason: "the artifact is larger than a page may be"}
	}
	reader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return ErrPagesArtifactInvalid{Reason: "the artifact is not a zip archive"}
	}
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		if !file.Mode().IsRegular() {
			continue
		}
		name := path.Clean("/" + strings.ReplaceAll(file.Name, "\\", "/"))
		if name == "/" || strings.HasPrefix(name, "/../") {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(name, "/")))
		if !strings.HasPrefix(target, dir+string(filepath.Separator)) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create directory for %q: %w", name, err)
		}
		if err := writeZipFile(file, target); err != nil {
			return err
		}
	}
	return nil
}

// maxPagesArtifactSize caps the artifact a deployment unpacks: pages are files a reader looks at,
// and they are not big.
const maxPagesArtifactSize = 256 * 1024 * 1024

func writeZipFile(file *zip.File, target string) error {
	source, err := file.Open()
	if err != nil {
		return fmt.Errorf("open %q in the artifact: %w", file.Name, err)
	}
	defer source.Close()
	destination, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %q: %w", target, err)
	}
	defer destination.Close()
	if _, err := io.Copy(destination, source); err != nil {
		return fmt.Errorf("write %q: %w", target, err)
	}
	return nil
}

// hasPagesIndex reports whether any directory of a freshly unpacked page carries an index.
func hasPagesIndex(dir string) bool {
	found := false
	err := filepath.WalkDir(dir, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == repo_model.PagesIndex {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return err == nil && found
}

// prunePagesDeployments keeps the history of a repository to its limit, oldest first to go.
func prunePagesDeployments(ctx context.Context, repoID int64) {
	old, err := repo_model.ListPagesDeployments(ctx, repoID, PagesDeploymentLimit+100)
	if err != nil || len(old) <= PagesDeploymentLimit {
		return
	}
	ids := make([]int64, 0, len(old)-PagesDeploymentLimit)
	for _, deployment := range old[PagesDeploymentLimit:] {
		ids = append(ids, deployment.ID)
	}
	if _, err := db.GetEngine(ctx).In("id", ids).Delete(&repo_model.PagesDeployment{}); err != nil {
		log.Error("prune pages deployments of repository %d: %v", repoID, err)
	}
}

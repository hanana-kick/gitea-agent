// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repository

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	actions_model "gitea.dev/models/actions"
	"gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/storage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPagesSettingsFoldIntoWhatCanBeServed(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})

	pages, err := SetPages(ctx, repo, PagesOptions{Source: repo_model.PagesSourceBranch, Dir: "docs"})
	require.NoError(t, err)
	assert.Equal(t, repo.DefaultBranch, pages.Branch, "a branch-sourced page falls back to the default branch")
	assert.Equal(t, "/docs", pages.Dir)

	pages, err = SetPages(ctx, repo, PagesOptions{Source: repo_model.PagesSourceBranch, Branch: "main", Dir: "/"})
	require.NoError(t, err)
	assert.Equal(t, "main", pages.Branch)
	assert.Equal(t, "/", pages.Dir)

	// another source keeps nothing of the branch it no longer reads
	pages, err = SetPages(ctx, repo, PagesOptions{Source: repo_model.PagesSourceActions, Branch: "main", Dir: "/docs"})
	require.NoError(t, err)
	assert.Empty(t, pages.Branch)
	assert.Empty(t, pages.Dir)

	_, err = SetPages(ctx, repo, PagesOptions{Source: "ftp"})
	assert.ErrorIs(t, err, ErrPagesArtifactInvalid{})
	_, err = SetPages(ctx, repo, PagesOptions{Source: repo_model.PagesSourceBranch, Dir: "/secrets"})
	assert.ErrorIs(t, err, ErrPagesArtifactInvalid{})

	// the settings page shows what was saved
	stored, has, err := repo_model.GetPages(ctx, repo.ID)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, repo_model.PagesSourceActions, stored.Source)

	require.NoError(t, RemovePages(ctx, repo))
	_, has, err = repo_model.GetPages(ctx, repo.ID)
	require.NoError(t, err)
	assert.False(t, has, "the repository publishes pages after they were unpublished")
}

// A deployment replaces what is served, and a deployment that can not be served leaves the one
// that was there in place and says why.
func TestDeployingPagesReplacesWhatIsServed(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	require.NoError(t, os.RemoveAll(PagesBuildDir(repo.ID)))
	t.Cleanup(func() { _ = os.RemoveAll(PagesBuildDir(repo.ID)) })

	first := pagesArtifact(t, repo.ID, map[string]string{"index.html": "first", "app.css": "body{}"})
	deployment, err := DeployPagesFromArtifact(ctx, repo, nil, first, 1)
	require.NoError(t, err)
	assert.Equal(t, repo_model.PagesStatusBuilt, deployment.Status)
	assert.Equal(t, first.CommitSHA, deployment.CommitSHA)
	assert.Equal(t, "first", string(readPagesFile(t, repo.ID, "index.html")))
	assert.Equal(t, "body{}", string(readPagesFile(t, repo.ID, "app.css")))

	// an artifact that holds no index is not a page: what was served stays served
	second := pagesArtifact(t, repo.ID, map[string]string{"readme.md": "notes"})
	_, err = DeployPagesFromArtifact(ctx, repo, nil, second, 1)
	assert.ErrorIs(t, err, ErrPagesArtifactInvalid{})
	assert.Equal(t, "first", string(readPagesFile(t, repo.ID, "index.html")))
	deployments, err := repo_model.ListPagesDeployments(ctx, repo.ID, PagesDeploymentLimit)
	require.NoError(t, err)
	require.NotEmpty(t, deployments)
	assert.Equal(t, repo_model.PagesStatusFailed, deployments[0].Status)
	assert.Contains(t, deployments[0].Error, "index.html")

	// a file of an artifact never lands outside the page directory
	escaping := pagesArtifact(t, repo.ID, map[string]string{"index.html": "third", "../escape.txt": "out", "/etc/passwd": "out"})
	_, err = DeployPagesFromArtifact(ctx, repo, nil, escaping, 1)
	require.NoError(t, err)
	assert.Equal(t, "third", string(readPagesFile(t, repo.ID, "index.html")))
	_, err = os.Stat(filepath.Join(filepath.Dir(PagesBuildDir(repo.ID)), "escape.txt"))
	assert.True(t, os.IsNotExist(err), "a file of the artifact was written beside the page")
	// a name with a leading separator is read as one inside the page, not at the root of the disk
	assert.Equal(t, "out", string(readPagesFile(t, repo.ID, "etc/passwd")))
}

// pageArtifactSeq names the artifacts of one test run apart.
var pageArtifactSeq atomic.Int64

// pagesArtifact stores a zip holding the given files as an artifact of the repository.
func pagesArtifact(t *testing.T, repoID int64, files map[string]string) *actions_model.ActionArtifact {
	t.Helper()
	ctx := t.Context()
	buffer := &bytes.Buffer{}
	archive := zip.NewWriter(buffer)
	for name, content := range files {
		writer, err := archive.Create(name)
		require.NoError(t, err)
		_, err = writer.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, archive.Close())

	seq := pageArtifactSeq.Add(1)
	artifact := &actions_model.ActionArtifact{
		RepoID: repoID, RunID: 0, ArtifactName: PagesArtifactName,
		Status: actions_model.ArtifactStatusUploadConfirmed, CommitSHA: "c0ffee",
		ArtifactPath: "pages-test/" + strconv.FormatInt(seq, 10),
		StoragePath:  "pages-test-" + strconv.FormatInt(repoID, 10) + "-" + strconv.FormatInt(seq, 10),
		FileSize:     int64(buffer.Len()), ContentEncodingOrType: actions_model.ContentTypeZip,
	}
	_, err := storage.ActionsArtifacts.Save(artifact.StoragePath, bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
	require.NoError(t, err)
	require.NoError(t, db.Insert(ctx, artifact))
	return artifact
}

func readPagesFile(t *testing.T, repoID int64, name string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(PagesBuildDir(repoID), filepath.FromSlash(name)))
	require.NoError(t, err)
	return content
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"context"
	"fmt"

	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
)

// PagesIndex is the file a directory of a published page is served by.
const PagesIndex = "index.html"

// Where a repository's pages come from. The branch kind is read live, the actions kind is
// whatever a workflow run deployed last.
const (
	PagesSourceBranch  = "branch"
	PagesSourceActions = "actions"
)

// IsValidPagesSource reports whether source names a way pages are published.
func IsValidPagesSource(source string) bool {
	switch source {
	case PagesSourceBranch, PagesSourceActions:
		return true
	}
	return false
}

// Pages is how one repository publishes its pages: from a branch, or from what a workflow
// run deployed. A repository has at most one.
type Pages struct {
	ID     int64  `xorm:"pk autoincr"`
	RepoID int64  `xorm:"UNIQUE NOT NULL"`
	Source string `xorm:"VARCHAR(16) NOT NULL"`
	// Branch and Dir say where a branch-sourced page is read: a directory of a branch, either
	// the repository root or the docs folder the way a forge offers it.
	Branch string `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"`
	Dir    string `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"`
	// CustomDomain is served through a proxy in front of the instance, so it is only recorded.
	CustomDomain string             `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"`
	CreatedUnix  timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix  timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (Pages) TableName() string {
	return "repo_pages"
}

// Pages deployment statuses.
const (
	PagesStatusBuilt  = "built"
	PagesStatusFailed = "failed"
)

// PagesDeployment is one deployment of a repository's pages, newest first in the history. The
// content of a branch deployment is read from the branch; an actions deployment carries the
// artifact it was unpacked from.
type PagesDeployment struct {
	ID     int64  `xorm:"pk autoincr"`
	RepoID int64  `xorm:"INDEX NOT NULL"`
	Source string `xorm:"VARCHAR(16) NOT NULL"`
	// CommitSHA is the commit the deployed content came from.
	CommitSHA string `xorm:"VARCHAR(64) NOT NULL DEFAULT ''"`
	// RunID and ArtifactID name the workflow run and artifact an actions deployment came from.
	RunID      int64  `xorm:"NOT NULL DEFAULT 0"`
	ArtifactID int64  `xorm:"NOT NULL DEFAULT 0"`
	Status     string `xorm:"VARCHAR(16) NOT NULL"`
	Error      string `xorm:"TEXT"`
	// DeployerID is who asked for the deployment; 0 when a workflow did.
	DeployerID  int64              `xorm:"NOT NULL DEFAULT 0"`
	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
}

func (PagesDeployment) TableName() string {
	return "repo_pages_deployment"
}

func init() {
	db.RegisterModel(new(Pages))
	db.RegisterModel(new(PagesDeployment))
}

// PagesURL is where the pages of a repository are served from.
func (repo *Repository) PagesURL() string {
	return repo.Link() + "/-/pages/"
}

// GetPages returns how a repository publishes pages, and whether it publishes any.
func GetPages(ctx context.Context, repoID int64) (*Pages, bool, error) {
	pages := &Pages{}
	has, err := db.GetEngine(ctx).Where("repo_id = ?", repoID).Get(pages)
	if err != nil {
		return nil, false, fmt.Errorf("get pages of repository %d: %w", repoID, err)
	}
	return pages, has, nil
}

// ListPagesDeployments returns the deployments of a repository, newest first.
func ListPagesDeployments(ctx context.Context, repoID int64, limit int) ([]*PagesDeployment, error) {
	deployments := make([]*PagesDeployment, 0, limit)
	if err := db.GetEngine(ctx).Where("repo_id = ?", repoID).OrderBy("id DESC").Limit(limit).Find(&deployments); err != nil {
		return nil, fmt.Errorf("list pages deployments of repository %d: %w", repoID, err)
	}
	return deployments, nil
}

// GetLatestPagesDeployment returns the deployment that is served, and whether there is one.
func GetLatestPagesDeployment(ctx context.Context, repoID int64) (*PagesDeployment, bool, error) {
	deployment := &PagesDeployment{}
	has, err := db.GetEngine(ctx).Where("repo_id = ?", repoID).
		And("status = ?", PagesStatusBuilt).OrderBy("id DESC").Get(deployment)
	if err != nil {
		return nil, false, fmt.Errorf("get latest pages deployment of repository %d: %w", repoID, err)
	}
	return deployment, has, nil
}

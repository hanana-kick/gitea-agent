// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package structs

import "time"

// Pages is how a repository publishes its pages.
// swagger:model
type Pages struct {
	// URL is where the published pages are served from.
	URL string `json:"url"`
	// Source is "branch" (read from a branch) or "actions" (deployed from a workflow run).
	Source string `json:"source"`
	// Branch and Dir say which directory of which branch a branch-sourced page is read from.
	Branch string `json:"branch"`
	Dir    string `json:"dir"`
	// CustomDomain is the domain a proxy in front of the instance serves the pages under.
	CustomDomain string `json:"custom_domain"`
	// CommitSHA is the commit of the content that is served, when anything is.
	CommitSHA string `json:"commit_sha"`
	// Deployment is the deployment that is served, for the actions source.
	Deployment *PagesDeployment `json:"deployment,omitempty"`
}

// PagesDeployment is one deployment of a repository's pages.
// swagger:model
type PagesDeployment struct {
	ID int64 `json:"id"`
	// Source is "branch" or "actions".
	Source string `json:"source"`
	// CommitSHA is the commit the deployed content came from.
	CommitSHA string `json:"commit_sha"`
	// RunID is the workflow run an actions deployment came from.
	RunID int64 `json:"run_id"`
	// RunIndex is the per-repository number of that run, when it is known.
	RunIndex int64 `json:"run_index"`
	// Status is "built" or "failed".
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// URL is the page of the workflow run that deployed, when there is one.
	URL       string    `json:"url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// UpdatePagesOption is how a repository publishes its pages.
// swagger:model
type UpdatePagesOption struct {
	// Source is "branch" or "actions".
	Source string `json:"source" binding:"Required;In(branch,actions)"`
	// Branch and Dir are where a branch-sourced page is read from.
	Branch string `json:"branch"`
	Dir    string `json:"dir"`
	// CustomDomain is the domain a proxy in front of the instance serves the pages under.
	CustomDomain string `json:"custom_domain"`
}

// DeployPagesOption is the artifact a repository's pages are deployed from.
// swagger:model
type DeployPagesOption struct {
	// ArtifactID is the artifact a workflow run uploaded, named "pages".
	ArtifactID int64 `json:"artifact_id" binding:"Required"`
}

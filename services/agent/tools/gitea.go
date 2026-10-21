// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gitea.dev/modules/markup"
	"gitea.dev/modules/markup/markdown"

	db_list "gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
)

// GiteaTools are the read tools of the instance. Every one runs with the calling user's
// permission, so a tool can not show what its user may not see.
type GiteaTools struct{}

func (GiteaTools) Registry() []Tool {
	return []Tool{
		searchIssuesTool{},
		readIssueTool{},
		listReposTool{},
	}
}

// searchIssuesTool finds issues across the repositories the user can see.
type searchIssuesTool struct{}

func (searchIssuesTool) Name() string { return "gitea_search_issues" }

func (searchIssuesTool) Description() string {
	return "Search Gitea issues by keyword. Use when the user asks about open work, bugs or tasks. " +
		"Returns issue titles, numbers and the repository they belong to."
}

func (searchIssuesTool) Scope() Scope { return ScopeUser }

func (searchIssuesTool) Args() map[string]string {
	return map[string]string{"keyword": "Text to search for in issue titles"}
}

func (searchIssuesTool) Run(ctx context.Context, doer *user_model.User, args json.RawMessage) (string, error) {
	var params struct {
		Keyword string `json:"keyword"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", err
	}
	params.Keyword = strings.TrimSpace(params.Keyword)
	if params.Keyword == "" {
		return "The keyword argument is required.", nil
	}
	// Issues() applies the permission scope of the doer, so the result stays inside
	// what the user may see
	issues, err := issues_model.Issues(ctx, &issues_model.IssuesOptions{
		Doer:      doer,
		AllPublic: true,
		SortType:  "newest",
		Paginator: &db_list.ListOptions{Page: 1, PageSize: 15},
	})
	if err != nil {
		return "", err
	}
	if len(issues) == 0 {
		return "No issues matched the keyword.", nil
	}
	lines := make([]string, 0, len(issues))
	for _, issue := range issues {
		repo, err := repo_model.GetRepositoryByID(ctx, issue.RepoID)
		if err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s #%d %s (%s)", repo.FullName(), issue.Index, issue.Title, issue.State()))
	}
	return strings.Join(lines, "\n"), nil
}

// readIssueTool returns the body and comments of one issue.
type readIssueTool struct{}

func (readIssueTool) Name() string { return "gitea_read_issue" }

func (readIssueTool) Description() string {
	return "Read one Gitea issue: title, body and its comments. Use after gitea_search_issues, " +
		"when the user asks for details of an issue."
}

func (readIssueTool) Scope() Scope { return ScopeUser }

func (readIssueTool) Args() map[string]string {
	return map[string]string{"issue_id": "The numeric issue ID from gitea_search_issues"}
}

func (readIssueTool) Run(ctx context.Context, doer *user_model.User, args json.RawMessage) (string, error) {
	var params struct {
		IssueID int64 `json:"issue_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", err
	}
	issue, err := issues_model.GetIssueByID(ctx, params.IssueID)
	if err != nil {
		return "", err
	}
	repo, err := repo_model.GetRepositoryByID(ctx, issue.RepoID)
	if err != nil {
		return "", err
	}
	perm, err := repo_model.GetRepositoryByID(ctx, issue.RepoID)
	if err != nil || perm == nil {
		return "That issue is not visible to you.", nil
	}
	out := []string{fmt.Sprintf("%s #%d %s (%s)", repo.FullName(), issue.Index, issue.Title, issue.State())}
	if issue.Content != "" {
		if rendered, err := markdown.RenderString(markup.NewRenderContext(ctx), issue.Content); err == nil {
			out = append(out, string(rendered))
		} else {
			out = append(out, issue.Content)
		}
	}
	return strings.Join(out, "\n\n"), nil
}

// listReposTool lists the repositories the calling user can see.
type listReposTool struct{}

func (listReposTool) Name() string { return "gitea_list_repos" }

func (listReposTool) Description() string {
	return "List the Gitea repositories visible to you. Use when the user asks what repositories exist " +
		"or which project to work on."
}

func (listReposTool) Scope() Scope { return ScopeUser }

func (listReposTool) Args() map[string]string { return map[string]string{} }

func (listReposTool) Run(ctx context.Context, doer *user_model.User, _ json.RawMessage) (string, error) {
	repos, _, err := repo_model.GetUserRepositories(ctx, repo_model.SearchRepoOptions{Actor: doer, ListOptions: db_list.ListOptions{Page: 1, PageSize: 20}})
	if err != nil {
		return "", err
	}
	if len(repos) == 0 {
		return "No repositories are visible to you.", nil
	}
	lines := make([]string, 0, len(repos))
	for _, repo := range repos {
		lines = append(lines, fmt.Sprintf("%s (%s)", repo.FullName(), repo.Description))
	}
	log.Trace("Agent tool gitea_list_repos returned %d repos for user %d", len(lines), doer.ID)
	return strings.Join(lines, "\n"), nil
}

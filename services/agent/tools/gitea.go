// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"strings"

	agent_model "gitea.dev/models/agent"
	db_list "gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	issue_indexer "gitea.dev/modules/indexer/issues"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/optional"
)

// toolOutputLimit caps what one tool puts in front of the model, so a long issue body
// cannot crowd the rest of the transcript out of the prompt.
const toolOutputLimit = 8 * 1024

// toolResultLimit caps how many rows a listing tool returns.
const toolResultLimit = 15

// GiteaTools are the read tools of the instance. Every one runs with the calling user's
// permission, so a tool can not show what its user may not see.
type GiteaTools struct{}

func (GiteaTools) Registry() []Tool {
	return []Tool{
		searchIssuesTool{},
		readIssueTool{},
		listReposTool{},
		readSkillTool{},
	}
}

// searchIssuesTool finds issues the user may see whose text matches a keyword.
type searchIssuesTool struct{}

func (searchIssuesTool) Name() string { return "gitea_search_issues" }

func (searchIssuesTool) Description() string {
	return "Search Gitea issues by keyword, or list the newest issues when no keyword is given. Use " +
		"when the user asks about open work, bugs or tasks. Returns issue titles, numbers, states " +
		"and the repository they belong to."
}

func (searchIssuesTool) Scope() Scope { return ScopeUser }

func (searchIssuesTool) Args() []Arg {
	return []Arg{{
		Name: "keyword", Optional: true,
		Description: "Text to look for in issue titles and bodies; leave it out to list the newest issues",
	}}
}

func (searchIssuesTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Keyword string `json:"keyword"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	params.Keyword = strings.TrimSpace(params.Keyword)
	issues, err := matchedIssues(ctx, doer, session.RepoID, params.Keyword)
	if err != nil {
		return "", err
	}
	if len(issues) == 0 {
		if params.Keyword == "" {
			return "No issues are visible here.", nil
		}
		return "No issues matched the keyword.", nil
	}
	lines := make([]string, 0, len(issues))
	for _, issue := range issues {
		if err := issue.LoadRepo(ctx); err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s #%d %s (%s)", issue.Repo.FullName(), issue.Index, issue.Title, issue.State()))
	}
	return clip(strings.Join(lines, "\n")), nil
}

// matchedIssues finds the issues the doer may see that mention the keyword; with no keyword
// it lists the newest ones instead. A repository session passes its repository, so neither
// ever leaves it. The index does the matching while it is running; otherwise the newest
// issues are filtered here, which only reaches as far as the listing does.
func matchedIssues(ctx context.Context, doer *user_model.User, repoID int64, keyword string) ([]*issues_model.Issue, error) {
	// no keyword asks what is there, not what matches: the listing is the answer itself
	listing := keyword == ""
	indexed := issue_indexer.IsAvailable(ctx) && !listing
	options := issues_model.IssuesOptions{
		Doer:      doer,
		AllPublic: true,
		IsPull:    optional.Some(false),
		SortType:  "newest",
	}
	if repoID != 0 {
		// the repository is the whole world of this session, so nothing outside it may match
		options.RepoIDs = []int64{repoID}
		options.AllPublic = false
	}
	if indexed {
		// the index only returns ids, so the pass through Issues() is what keeps the
		// result inside what the doer may see
		ids, _, err := issue_indexer.SearchIssues(ctx, issue_indexer.ToSearchOptions(keyword, options.Copy()))
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, nil
		}
		if len(ids) > toolResultLimit {
			ids = ids[:toolResultLimit]
		}
		options.IssueIDs = ids
	}
	options.Paginator = &db_list.ListOptions{Page: 1, PageSize: toolResultLimit}
	issues, err := issues_model.Issues(ctx, &options)
	if err != nil {
		return nil, err
	}
	if indexed || listing {
		return issues, nil
	}
	wanted := strings.ToLower(keyword)
	matched := make([]*issues_model.Issue, 0, len(issues))
	for _, issue := range issues {
		if strings.Contains(strings.ToLower(issue.Title), wanted) {
			matched = append(matched, issue)
		}
	}
	return matched, nil
}

// readIssueTool returns the body and comments of one issue.
type readIssueTool struct{}

func (readIssueTool) Name() string { return "gitea_read_issue" }

func (readIssueTool) Description() string {
	return "Read one Gitea issue: its title, state and body. Use after gitea_search_issues to look at " +
		"an issue in full."
}

func (readIssueTool) Scope() Scope { return ScopeUser }

func (readIssueTool) Args() []Arg {
	return []Arg{{Name: "issue_id", Description: "The numeric issue ID from gitea_search_issues", Type: "integer"}}
}

func (readIssueTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		IssueID int64 `json:"issue_id"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	if params.IssueID == 0 {
		return "The issue_id argument is required.", nil
	}
	issue, err := issues_model.GetIssueByID(ctx, params.IssueID)
	if err != nil {
		return "There is no issue with that ID.", nil
	}
	if session.RepoID != 0 && issue.RepoID != session.RepoID {
		return "That issue belongs to another repository, so search this one instead.", nil
	}
	if err := issue.LoadRepo(ctx); err != nil {
		return "", err
	}
	// the doer has to be able to read the issue where it lives, not just know its ID
	perm, err := access_model.GetIndividualUserRepoPermission(ctx, issue.Repo, doer)
	if err != nil {
		return "", err
	}
	if !perm.CanReadIssuesOrPulls(issue.IsPull) {
		return "That issue is not visible to you.", nil
	}
	out := []string{fmt.Sprintf("%s #%d %s (%s)", issue.Repo.FullName(), issue.Index, issue.Title, issue.State())}
	if issue.Content != "" {
		// the model reads markdown itself, so the stored source costs it fewer tokens than rendered HTML
		out = append(out, issue.Content)
	}
	return clip(strings.Join(out, "\n\n")), nil
}

// listReposTool lists the repositories the calling user can see.
type listReposTool struct{}

func (listReposTool) Name() string { return "gitea_list_repos" }

func (listReposTool) Description() string {
	return "List the Gitea repositories visible to you. Use when the user asks what repositories exist " +
		"or which project to work on."
}

func (listReposTool) Scope() Scope { return ScopeUser }

func (listReposTool) Args() []Arg { return nil }

// Offered keeps the list out of a repository session: that session already works inside
// one repository, so a list of the user's repositories has nothing to offer it.
func (listReposTool) Offered(_ context.Context, session Session) (bool, error) {
	return session.Kind != agent_model.ConversationKindRepository, nil
}

func (listReposTool) Run(ctx context.Context, doer *user_model.User, _ Session, _ string) (string, error) {
	repos, _, err := repo_model.GetUserRepositories(ctx, repo_model.SearchRepoOptions{
		Actor:       doer,
		ListOptions: db_list.ListOptions{Page: 1, PageSize: toolResultLimit},
	})
	if err != nil {
		return "", err
	}
	if len(repos) == 0 {
		return "No repositories are visible to you.", nil
	}
	lines := make([]string, 0, len(repos))
	for _, repo := range repos {
		line := repo.FullName()
		if repo.Description != "" {
			line += ": " + strings.TrimSpace(repo.Description)
		}
		lines = append(lines, line)
	}
	log.Trace("Agent tool gitea_list_repos returned %d repos for user %d", len(lines), doer.ID)
	return clip(strings.Join(lines, "\n")), nil
}

// clip cuts tool output down to what the prompt can afford.
func clip(output string) string {
	if len(output) <= toolOutputLimit {
		return output
	}
	cut := strings.LastIndex(output[:toolOutputLimit], "\n")
	if cut < toolOutputLimit/2 {
		cut = toolOutputLimit
	}
	return output[:cut] + "\n[truncated]"
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"

	agent_model "gitea.dev/models/agent"
	db_list "gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	unit_model "gitea.dev/models/unit"
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
		listFilesTool{},
		readFileTool{},
		listRunsTool{},
		readPullRequestTool{},
		listCommitsTool{},
		readCommitTool{},
		listBranchesTool{},
		searchCodeTool{},
		commitFilesTool{},
		reviewPullRequestTool{},
		readReviewsTool{},
		commentPullRequestTool{},
	}
}

// searchIssuesTool finds issues the user may see whose text matches a keyword.
type searchIssuesTool struct{}

func (searchIssuesTool) Name() string { return "gitea_search_issues" }

func (searchIssuesTool) Description() string {
	return "Search this Gitea instance's issues and pull requests by keyword, or list the newest when " +
		"no keyword is given. Open ones are returned unless another state is asked for. Use it when " +
		"the user asks about open work, bugs, reviews or tasks."
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
		Keyword    string `json:"keyword"`
		Repository string `json:"repository"`
		State      string `json:"state"`
		Kind       string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	params.Keyword = strings.TrimSpace(params.Keyword)
	state, err := parseIssueState(params.State)
	if err != nil {
		if message, told := toldToModel(err); told {
			return message, nil
		}
		return "", err
	}
	kind, err := parseIssueKind(params.Kind)
	if err != nil {
		if message, told := toldToModel(err); told {
			return message, nil
		}
		return "", err
	}
	repoID, err := searchScope(ctx, doer, session, params.Repository)
	if err != nil {
		if message, told := toldToModel(err); told {
			return message, nil
		}
		return "", err
	}
	issues, err := matchedIssues(ctx, doer, repoID, params.Keyword, state, kind)
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
		lines = append(lines, issueLine(issue))
	}
	return clip(strings.Join(lines, "\n")), nil
}

// issueState is what a search asks for: open ones, closed ones, or either.
type issueState int

const (
	issueStateOpen issueState = iota
	issueStateClosed
	issueStateAll
)

// parseIssueState reads the state a call asked for, defaulting to open work.
func parseIssueState(raw string) (issueState, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "open":
		return issueStateOpen, nil
	case "closed":
		return issueStateClosed, nil
	case "all":
		return issueStateAll, nil
	}
	return issueStateOpen, unreadableRepo{`The state is "open", "closed" or "all".`}
}

// issueKind is what a search asks for among the issues and pull requests of an instance.
type issueKind int

const (
	issueKindEither issueKind = iota
	issueKindIssue
	issueKindPull
)

// parseIssueKind reads the kind a call asked for, defaulting to both: an issue and a pull
// request are both open work, and a task rarely cares which it is.
func parseIssueKind(raw string) (issueKind, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "either", "both", "all":
		return issueKindEither, nil
	case "issue", "issues":
		return issueKindIssue, nil
	case "pull", "pulls", "pull request", "pull requests", "pr", "prs":
		return issueKindPull, nil
	}
	return issueKindEither, unreadableRepo{`The kind is "issue", "pull request", or left out for both.`}
}

// issueLine names an issue the way the transcript and the tools speak of it.
func issueLine(issue *issues_model.Issue) string {
	line := fmt.Sprintf("%s #%d %s (%s", issue.Repo.FullName(), issue.Index, issue.Title, issue.State())
	if issue.IsPull {
		line += ", pull request"
	}
	return line + ")"
}

// matchedIssues finds the issues the doer may see that mention the keyword; with no keyword
// it lists the newest ones instead. A repository session passes its repository, so neither
// ever leaves it. The index does the matching while it is running; otherwise the newest
// issues are filtered here, which only reaches as far as the listing does.
func matchedIssues(ctx context.Context, doer *user_model.User, repoID int64, keyword string, state issueState, kind issueKind) ([]*issues_model.Issue, error) {
	// no keyword asks what is there, not what matches: the listing is the answer itself
	listing := keyword == ""
	indexed := issue_indexer.IsAvailable(ctx) && !listing
	options := issues_model.IssuesOptions{
		Doer:      doer,
		AllPublic: true,
		SortType:  "newest",
	}
	switch kind {
	case issueKindIssue:
		options.IsPull = optional.Some(false)
	case issueKindPull:
		options.IsPull = optional.Some(true)
	}
	switch state {
	case issueStateOpen:
		options.IsClosed = optional.Some(false)
	case issueStateClosed:
		options.IsClosed = optional.Some(true)
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
	return "Read one issue or pull request: its title, state, body, its discussion, and for a pull " +
		"request the branches it merges. Use the number gitea_search_issues showed for it."
}

func (readIssueTool) Scope() Scope { return ScopeUser }

func (readIssueTool) Args() []Arg {
	return []Arg{
		repositoryArg("the issue or pull request it reads"),
		{Name: "index", Type: "integer", Description: "The number gitea_search_issues showed for it"},
	}
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

func (readIssueTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Index      int64  `json:"index"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	if params.Index == 0 {
		return "The index argument is required.", nil
	}
	repository, err := resolveRepo(ctx, doer, session, params.Repository, unit_model.TypeIssues)
	if err != nil {
		if message, told := toldToModel(err); told {
			return message, nil
		}
		return "", err
	}
	issue, err := issues_model.GetIssueByIndex(ctx, repository.ID, params.Index)
	if err != nil {
		return "There is no issue or pull request with that number in " + repository.FullName() + ".", nil
	}
	if err := issue.LoadRepo(ctx); err != nil {
		return "", err
	}
	if issue.IsPull {
		if err := issue.LoadPullRequest(ctx); err != nil {
			return "", err
		}
		if err := issue.PullRequest.LoadAttributes(ctx); err != nil {
			return "", err
		}
	}
	// the repository has to have the unit this thing belongs to, and the user has to be able
	// to read it there
	perm, err := access_model.GetIndividualUserRepoPermission(ctx, issue.Repo, doer)
	if err != nil {
		return "", err
	}
	if !perm.CanReadIssuesOrPulls(issue.IsPull) {
		return "That is not visible to you.", nil
	}
	out := []string{issueLine(issue)}
	if issue.IsPull {
		out = append(out, pullRequestLine(issue.PullRequest))
	}
	if issue.Content != "" {
		// the model reads markdown itself, so the stored source costs it fewer tokens than rendered HTML
		out = append(out, issue.Content)
	}
	discussion, err := issueDiscussion(ctx, issue)
	if err != nil {
		return "", err
	}
	if discussion != "" {
		out = append(out, discussion)
	}
	return clip(strings.Join(out, "\n\n")), nil
}

// issueDiscussion reads what was said on an issue: an issue is its discussion as much as the
// body it was opened with, and the newest words are what became of it.
func issueDiscussion(ctx context.Context, issue *issues_model.Issue) (string, error) {
	// what was said, not what happened: labels, references and pushes are the bookkeeping of
	// an issue, not a word in it
	wanted := []issues_model.CommentType{
		issues_model.CommentTypeComment, issues_model.CommentTypeCode, issues_model.CommentTypeReview,
	}
	total, err := db_list.GetEngine(ctx).Where("issue_id = ?", issue.ID).
		In("type", issues_model.CommentTypeComment, issues_model.CommentTypeCode, issues_model.CommentTypeReview).
		Count(new(issues_model.Comment))
	if err != nil {
		return "", err
	}
	if total == 0 {
		return "", nil
	}
	// the newest words are the last page of them
	const pageSize = 30
	comments, err := issues_model.FindComments(ctx, &issues_model.FindCommentsOptions{
		IssueID: issue.ID,
		// the zero type is a plain comment, not "any": asking for every type is what leaves the
		// bookkeeping in, which is then dropped below
		Type:        issues_model.CommentTypeUndefined,
		ListOptions: db_list.ListOptions{Page: int((total-1)/pageSize + 1), PageSize: pageSize},
	})
	if err != nil {
		return "", err
	}
	said := make([]string, 0, len(comments))
	for _, comment := range comments {
		if !slices.Contains(wanted, comment.Type) {
			continue
		}
		if err := comment.LoadPoster(ctx); err != nil {
			return "", err
		}
		author := "someone"
		if comment.Poster != nil {
			author = comment.Poster.Name
		}
		if comment.Type == issues_model.CommentTypeCode && comment.TreePath != "" {
			author += fmt.Sprintf(" (on %s:%d)", comment.TreePath, comment.Line)
		}
		said = append(said, "- "+author+": "+strings.TrimSpace(comment.Content))
	}
	if len(said) == 0 {
		return "", nil
	}
	if len(said) > 20 {
		said = said[len(said)-20:]
	}
	return fmt.Sprintf("%d comments, the last %d of them:\n%s", total, len(said), strings.Join(said, "\n")), nil
}

// pullRequestLine says what a pull request merges where, which is what reading one is for.
func pullRequestLine(pr *issues_model.PullRequest) string {
	head := pr.HeadBranch
	if pr.HeadRepo != nil {
		head = pr.HeadRepo.FullName() + ":" + pr.HeadBranch
	}
	base := pr.BaseBranch
	if pr.BaseRepo != nil {
		base = pr.BaseRepo.FullName() + ":" + pr.BaseBranch
	}
	return "merges " + head + " into " + base
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

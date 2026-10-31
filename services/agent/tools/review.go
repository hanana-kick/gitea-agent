// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"

	issues_model "gitea.dev/models/issues"
	unit_model "gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	issues_service "gitea.dev/services/issue"
	pull_service "gitea.dev/services/pull"
)

// reviewPullRequestTool reads a pull request through a reviewer's eyes: it writes a review of
// the change, and the verdict is the reader's, made on what the tools read.
type reviewPullRequestTool struct{}

func (reviewPullRequestTool) Name() string { return "gitea_review_pull_request" }

func (reviewPullRequestTool) Description() string {
	return "Leave a review on a pull request: approve it, ask for changes, or comment. The verdict " +
		"is the reader's own, so give it the words they would sign. gitea_read_pull_request reads " +
		"the change, gitea_read_issue the description."
}

func (reviewPullRequestTool) Scope() Scope { return ScopeRepository }

func (reviewPullRequestTool) Writes() bool { return true }

func (reviewPullRequestTool) Args() []Arg {
	return []Arg{
		repositoryArg("the pull request"),
		{Name: "index", Type: "integer", Description: "The number gitea_search_issues showed for it"},
		{Name: "verdict", Description: `"approve", "request_changes" or "comment"`},
		{Name: "body", Description: "The review text"},
	}
}

func (reviewPullRequestTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := reviewPullRequestTool{}.review(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (reviewPullRequestTool) review(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Index      int64  `json:"index"`
		Verdict    string `json:"verdict"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	repository, err := resolveRepo(ctx, doer, session, params.Repository, unit_model.TypePullRequests)
	if err != nil {
		return "", err
	}
	issue, err := issues_model.GetIssueByIndex(ctx, repository.ID, params.Index)
	if err != nil {
		return "There is no issue or pull request with that number in " + repository.FullName() + ".", nil
	}
	if !issue.IsPull {
		return fmt.Sprintf("That is issue #%d, not a pull request: it has nothing to review. Read it with %s.",
			issue.Index, readIssueToolName), nil
	}
	if err := issue.LoadRepo(ctx); err != nil {
		return "", err
	}
	// a review of one's own pull request is refused by the service, and said here in words
	if issue.PosterID == doer.ID {
		return "That pull request is yours: you cannot approve or reject it yourself.", nil
	}

	verdict, err := parseVerdict(params.Verdict)
	if err != nil {
		if message, told := toldToModel(err); told {
			return message, nil
		}
		return "", err
	}
	gitRepo, err := git.OpenRepository(ctx, repository)
	if err != nil {
		return "", err
	}
	defer gitRepo.Close()

	review, comment, err := pull_service.SubmitReview(ctx, doer, gitRepo, issue, verdict,
		strings.TrimSpace(params.Body), issue.PullRequest.GetGitHeadRefName(), nil)
	if err != nil {
		return "", err
	}
	log.Trace("Agent tool gitea_review_pull_request wrote a %s review on %s#%d for user %d",
		verdict, repository.FullName(), issue.Index, doer.ID)
	said := ""
	if comment != nil && comment.Content != "" {
		said = comment.Content
	} else if review != nil {
		said = review.Content
	}
	out := fmt.Sprintf("Reviewed %s #%d: %s", repository.FullName(), issue.Index, verdictText(review))
	if said != "" {
		out += "\n" + said
	}
	return clip(out), nil
}

// parseVerdict reads the verdict a call asked for.
func parseVerdict(raw string) (issues_model.ReviewType, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "approve":
		return issues_model.ReviewTypeApprove, nil
	case "request_changes", "reject":
		return issues_model.ReviewTypeReject, nil
	case "comment", "comment_only":
		return issues_model.ReviewTypeComment, nil
	}
	return issues_model.ReviewTypeUnknown, unreadableRepo{`The verdict is "approve", "request_changes" or "comment".`}
}

// verdictText names a review the way a transcript reads it.
func verdictText(review *issues_model.Review) string {
	if review == nil {
		return "no review was recorded"
	}
	switch review.Type {
	case issues_model.ReviewTypeApprove:
		return "approved"
	case issues_model.ReviewTypeReject:
		return "changes requested"
	case issues_model.ReviewTypeComment:
		return "commented"
	case issues_model.ReviewTypePending:
		return "review pending"
	}
	return "reviewed"
}

// readReviewsTool reads the reviews a pull request has, so a review is written knowing what was
// already said and by whom.
type readReviewsTool struct{}

func (readReviewsTool) Name() string { return "gitea_read_pull_request_reviews" }

func (readReviewsTool) Description() string {
	return "List the reviews of a pull request: verdict, author and text, newest last. Use it before " +
		"writing a review, so the same words are not said twice."
}

func (readReviewsTool) Scope() Scope { return ScopeRepository }

func (readReviewsTool) Args() []Arg {
	return []Arg{
		repositoryArg("the pull request"),
		{Name: "index", Type: "integer", Description: "The number gitea_search_issues showed for it"},
	}
}

func (readReviewsTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := readReviewsTool{}.read(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (readReviewsTool) read(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Index      int64  `json:"index"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	repository, err := resolveRepo(ctx, doer, session, params.Repository, unit_model.TypePullRequests)
	if err != nil {
		return "", err
	}
	issue, err := issues_model.GetIssueByIndex(ctx, repository.ID, params.Index)
	if err != nil {
		return "There is no issue or pull request with that number in " + repository.FullName() + ".", nil
	}
	if !issue.IsPull {
		return fmt.Sprintf("That is issue #%d, not a pull request: it has no reviews.", issue.Index), nil
	}
	reviews, err := issues_model.FindReviews(ctx, issues_model.FindReviewOptions{IssueID: issue.ID})
	if err != nil {
		return "", err
	}
	reviews = slices.DeleteFunc(reviews, func(review *issues_model.Review) bool {
		return review.Type == issues_model.ReviewTypePending
	})
	if len(reviews) == 0 {
		return fmt.Sprintf("Pull request %s #%d has no reviews yet.", repository.FullName(), issue.Index), nil
	}
	lines := make([]string, 0, len(reviews))
	for _, review := range reviews {
		if err := review.LoadReviewer(ctx); err != nil {
			continue
		}
		name := review.Reviewer.Name

		line := fmt.Sprintf("- %s: %s", name, verdictText(review))
		if strings.TrimSpace(review.Content) != "" {
			if len(review.Content) > 200 {
				line += ": " + review.Content[:200] + "…"
			} else {
				line += ": " + review.Content
			}
		}
		lines = append(lines, line)
	}
	log.Trace("Agent tool gitea_read_pull_request_reviews listed %d reviews of %s#%d for user %d",
		len(reviews), repository.FullName(), issue.Index, doer.ID)
	return clip(strings.Join(lines, "\n")), nil
}

// commentPullRequestTool answers on a pull request: the discussion a review belongs to.
type commentPullRequestTool struct{}

func (commentPullRequestTool) Name() string { return "gitea_comment_issue" }

func (commentPullRequestTool) Description() string {
	return "Comment on an issue or pull request. Use it to add what you found to the discussion; " +
		"a verdict on a pull request is a review, not a comment."
}

func (commentPullRequestTool) Scope() Scope { return ScopeRepository }

func (commentPullRequestTool) Writes() bool { return true }

func (commentPullRequestTool) Args() []Arg {
	return []Arg{
		repositoryArg("the issue or pull request"),
		{Name: "index", Type: "integer", Description: "The number gitea_search_issues showed for it"},
		{Name: "body", Description: "The comment text"},
	}
}

func (commentPullRequestTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := commentPullRequestTool{}.comment(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (commentPullRequestTool) comment(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Index      int64  `json:"index"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	repository, err := resolveRepo(ctx, doer, session, params.Repository, unit_model.TypeIssues)
	if err != nil {
		return "", err
	}
	issue, err := issues_model.GetIssueByIndex(ctx, repository.ID, params.Index)
	if err != nil {
		return "There is no issue or pull request with that number in " + repository.FullName() + ".", nil
	}
	body := strings.TrimSpace(params.Body)
	if body == "" {
		return "The body argument is required: an empty comment says nothing.", nil
	}
	comment, err := issues_service.CreateIssueComment(ctx, doer, repository, issue, body, nil)
	if err != nil {
		return "", err
	}
	log.Trace("Agent tool gitea_comment_issue commented on %s#%d for user %d", repository.FullName(), issue.Index, doer.ID)
	return fmt.Sprintf("Commented on %s #%d (comment %d).", repository.FullName(), issue.Index, comment.ID), nil
}

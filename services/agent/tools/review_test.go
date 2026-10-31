package tools

import (
	"context"
	"testing"

	agent_model "gitea.dev/models/agent"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A review is written with the calling user's rights, and the verdict is what the model asked
// for: a pull request that is not one, a verdict that is not a word, a review of one's own pull
// request, are all answered rather than run.
func TestReviewAnswersInsteadOfRunning(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	_, err := readReviewsTool{}.Run(ctx, owner, session, `{"index":2}`)
	require.NoError(t, err)

	bad, err := reviewPullRequestTool{}.Run(ctx, owner, session, `{"index":2,"verdict":"maybe"}`)
	require.NoError(t, err)
	assert.Contains(t, bad, `"approve", "request_changes" or "comment"`)

	issue, err := reviewPullRequestTool{}.Run(ctx, owner, session, `{"index":1,"verdict":"approve"}`)
	require.NoError(t, err)
	assert.Contains(t, issue, "not a pull request")

	reviews, err := readReviewsTool{}.Run(ctx, owner, session, `{"index":3}`)
	require.NoError(t, err)
	assert.Contains(t, reviews, "changes requested") // the fixture PR has reviews of both kinds

	unnamed, err := commentPullRequestTool{}.Run(ctx, owner, session, `{"index":1,"body":" "}`)
	require.NoError(t, err)
	assert.Contains(t, unnamed, "The body argument is required: an empty comment says nothing.")

	_ = repo_model.Repository{}
}

// A review needs the change it is about: a pull request the service cannot line up is answered
// rather than crashing the tool.
func TestReviewAnswersAPullRequestWithNoChange(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	out, err := reviewPullRequestTool{}.Run(ctx, owner, session, `{"index":2,"verdict":"comment","body":"x"}`)
	require.NoError(t, err)
	assert.NotPanics(t, func() {
		_, _ = reviewPullRequestTool{}.Run(ctx, owner, session, `{"index":2,"verdict":"approve","body":"x"}`)
	})
	assert.NotEmpty(t, out)
}

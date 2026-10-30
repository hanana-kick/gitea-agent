// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"

	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	unit_model "gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
)

// repositoryNameArg is the argument a session that spans the instance uses to say which
// repository it means; a session bound to one repository leaves it out.
const repositoryNameArg = "repository"

// repositoryArg asks for the repository of a call, in the words the session it is offered in
// needs to hear.
func repositoryArg(what string) Arg {
	return Arg{
		Name: repositoryNameArg, Optional: true,
		Description: "Repository to look at, as owner/name. A session that already works in one " +
			"repository leaves this out and reads " + what + " of that repository.",
	}
}

// resolveRepo works out which repository a call means, and whether the user behind the
// session may read what the call is about. A session bound to a repository reads only that
// one, so its world never widens; a session that spans the instance names the repository it
// means, and is held to what its user can see in it.
func resolveRepo(ctx context.Context, doer *user_model.User, session Session, name string, unit unit_model.Type) (*repo_model.Repository, error) {
	name = strings.TrimSpace(name)
	if session.RepoID != 0 {
		repository, err := repo_model.GetRepositoryByID(ctx, session.RepoID)
		if err != nil {
			return nil, err
		}
		if name != "" && !strings.EqualFold(name, repository.FullName()) {
			return nil, unreadableRepo{"This session works in " + repository.FullName() + ", so it can only look at that repository."}
		}
		if !access_model.CheckRepoUnitUser(ctx, repository, doer, unit) {
			return nil, unreadableRepo{"You cannot read the " + unitNoun(unit) + " of " + repository.FullName() + "."}
		}
		return repository, nil
	}
	if name == "" {
		return nil, unreadableRepo{"This session is not bound to a repository, so say which one you mean as owner/name."}
	}
	owner, repoName, ok := strings.Cut(name, "/")
	if !ok || owner == "" || repoName == "" {
		return nil, unreadableRepo{"Give the repository as owner/name, the way gitea_list_repos shows it."}
	}
	repository, err := repo_model.GetRepositoryByOwnerAndName(ctx, owner, repoName)
	if err != nil {
		if repo_model.IsErrRepoNotExist(err) {
			return nil, unreadableRepo{"There is no repository " + name + " here."}
		}
		return nil, err
	}
	if !access_model.CheckRepoUnitUser(ctx, repository, doer, unit) {
		return nil, unreadableRepo{"You cannot read the " + unitNoun(unit) + " of " + name + "."}
	}
	return repository, nil
}

// searchScope is the repository a search is confined to: the one the session works in, the
// one it named, or none at all, which reaches as far as the user can see.
func searchScope(ctx context.Context, doer *user_model.User, session Session, name string) (int64, error) {
	if name == "" {
		return session.RepoID, nil
	}
	repository, err := resolveRepo(ctx, doer, session, name, unit_model.TypeIssues)
	if err != nil {
		return 0, err
	}
	return repository.ID, nil
}

// unitNoun names a unit the way a refusal reads.
func unitNoun(unit unit_model.Type) string {
	switch unit {
	case unit_model.TypeIssues:
		return "issues"
	case unit_model.TypeActions:
		return "actions"
	}
	return "code"
}

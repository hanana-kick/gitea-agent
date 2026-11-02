// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"strings"

	repo_model "gitea.dev/models/repo"
	unit_model "gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	release_service "gitea.dev/services/release"
)

// createReleaseTool publishes a version of a repository: the tag, the words that go with it, and
// the place a reader finds it. A change that is only committed is not yet handed over; a release
// is.
type createReleaseTool struct{}

func (createReleaseTool) Name() string { return "gitea_create_release" }

func (createReleaseTool) Description() string {
	return "Publish a release: tag the branch it is made from, with a title and notes, so a reader " +
		"can see what shipped and download it. Leave the notes out and they are written from the " +
		"commits since the last release. Draft and prerelease releases are marked as such."
}

func (createReleaseTool) Scope() Scope { return ScopeRepository }

func (createReleaseTool) Writes() bool { return true }

func (createReleaseTool) Args() []Arg {
	return []Arg{
		repositoryArg("the release"),
		{Name: "tag", Description: "The tag to publish, like v1.0.0"},
		{Name: "title", Description: "The name of the release"},
		{Name: "notes", Optional: true, Description: "What the release says; written from the commits when this is left out"},
		{Name: "target", Optional: true, Description: "The branch the release is made from; leave it out for the default branch"},
		{Name: "prerelease", Optional: true, Type: "boolean", Description: "Mark it as a prerelease"},
		{Name: "draft", Optional: true, Type: "boolean", Description: "Keep it as a draft until a reader publishes it"},
	}
}

func (createReleaseTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := createReleaseTool{}.create(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (createReleaseTool) create(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Tag        string `json:"tag"`
		Title      string `json:"title"`
		Notes      string `json:"notes"`
		Target     string `json:"target"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	tag := strings.TrimSpace(params.Tag)
	title := strings.TrimSpace(params.Title)
	if tag == "" || title == "" {
		return "The tag and title arguments are required.", nil
	}
	actor, err := actorOf(ctx, doer, session)
	if err != nil {
		return "", err
	}
	repository, err := resolveRepo(ctx, actor, session, params.Repository, unit_model.TypeCode)
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(params.Target)
	if target == "" {
		target = repository.DefaultBranch
	}
	gitRepo, err := git.OpenRepository(ctx, repository)
	if err != nil {
		return "", err
	}
	defer gitRepo.Close()
	if !git.IsBranchExist(ctx, gitRepo, target) {
		return fmt.Sprintf("The branch %s is not in %s.", target, repository.FullName()), nil
	}
	// a tag that is taken is not worth writing notes for
	if taken, err := repo_model.IsReleaseExist(ctx, repository.ID, tag); err != nil {
		return "", err
	} else if taken {
		return fmt.Sprintf("%s already has a release or tag named %s: choose the next version.",
			repository.FullName(), tag), nil
	}
	notes := strings.TrimSpace(params.Notes)
	if notes == "" {
		// the commits since the last release are what a reader wants to know about
		generated, err := release_service.GenerateReleaseNotes(ctx, repository, gitRepo, release_service.GenerateReleaseNotesOptions{
			TagName:   tag,
			TagTarget: target,
		})
		if err != nil {
			return "", err
		}
		notes = generated
	}
	release := &repo_model.Release{
		RepoID:       repository.ID,
		Repo:         repository,
		PublisherID:  actor.ID,
		Publisher:    actor,
		Title:        title,
		TagName:      tag,
		Target:       target,
		Note:         notes,
		IsDraft:      params.Draft,
		IsPrerelease: params.Prerelease,
	}
	if err := release_service.CreateRelease(ctx, gitRepo, release, nil, ""); err != nil {
		switch {
		case release_service.IsErrTagAlreadyExists(err), repo_model.IsErrReleaseAlreadyExist(err):
			return fmt.Sprintf("%s already has a release or tag named %s: choose the next version, and read the last one with gitea_list_releases.",
				repository.FullName(), tag), nil
		case release_service.IsErrInvalidTagName(err):
			return fmt.Sprintf("%q is not a tag a repository can hold: use a name like v1.0.0.", tag), nil
		case release_service.IsErrProtectedTagName(err):
			return fmt.Sprintf("%s protects the pattern of the tag %s: publish under a name the protection rule allows.", repository.FullName(), tag), nil
		}
		return "", err
	}
	log.Trace("Agent tool gitea_create_release published %s of %s for user %d", tag, repository.FullName(), doer.ID)
	state := ""
	if params.Draft {
		state = " (draft, so a reader publishes it)"
	} else if params.Prerelease {
		state = " (prerelease)"
	}
	published := ""
	if entry, err := gitRepo.GetBranchCommit(ctx, target); err == nil {
		if entry, err := entry.GetTreeEntryByPath(ctx, gitRepo, "public/index.html"); err == nil && !entry.IsDir() {
			published = fmt.Sprintf("\nThe page this repository publishes is served at %s/-/site/.", repository.Link())
		}
	}
	return fmt.Sprintf("Released %s in %s from %s%s: %s/releases/tag/%s\n%s%s",
		tag, repository.FullName(), target, state, repository.Link(), tag, notes, published), nil
}

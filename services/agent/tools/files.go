// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	unit_model "gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/typesniffer"
	files_service "gitea.dev/services/repository/files"
)

// fileListLimit caps how many entries one listing shows, so a large tree is cut at a whole
// line rather than in the middle of one.
const fileListLimit = 200

// unreadableRepo is a session with no repository to read, or one its user may not read. What
// it says is meant for the model, which is told to look elsewhere instead of guessing.
type unreadableRepo struct{ reason string }

func (err unreadableRepo) Error() string { return err.reason }

// sessionRepo is the repository of a session at one revision, opened with the rights of the
// user who asked for it.
type sessionRepo struct {
	repository *repo_model.Repository
	gitRepo    *git.Repository
	commit     *git.Commit
	ref        string
}

// openSessionRepo opens what a session may read: the repository it works in, at the revision
// it asked for. A tool reads with the rights of the calling user, so what the user may not
// see, the model may not see either.
func openSessionRepo(ctx context.Context, doer *user_model.User, session Session, ref string) (*sessionRepo, error) {
	if session.RepoID == 0 {
		return nil, unreadableRepo{"This session is not bound to a repository, so it has no files to read."}
	}
	repository, err := repo_model.GetRepositoryByID(ctx, session.RepoID)
	if err != nil {
		return nil, err
	}
	if !access_model.CheckRepoUnitUser(ctx, repository, doer, unit_model.TypeCode) {
		return nil, unreadableRepo{"You cannot read the code of this repository."}
	}
	gitRepo, err := git.OpenRepository(ctx, repository)
	if err != nil {
		return nil, err
	}
	if ref = strings.TrimSpace(ref); ref == "" {
		ref = repository.DefaultBranch
	}
	commit, err := gitRepo.GetCommit(ctx, ref)
	if err != nil {
		_ = gitRepo.Close()
		// an unknown revision is something the model can correct, unlike a broken repository
		if git.IsErrNotExist(err) {
			return nil, unreadableRepo{"There is no revision named " + ref + " in this repository."}
		}
		return nil, err
	}
	return &sessionRepo{repository: repository, gitRepo: gitRepo, commit: commit, ref: ref}, nil
}

// entryAt finds one path in the revision, telling the model when there is nothing there.
func (s *sessionRepo) entryAt(ctx context.Context, path string) (*git.TreeEntry, string, error) {
	cleaned := files_service.CleanGitTreePath(path)
	// a path that cleans away to nothing is either the root or one a repository cannot hold
	if cleaned == "" && !isRootPath(path) {
		return nil, "", unreadableRepo{"That path is one a repository cannot hold."}
	}
	entry, err := s.commit.GetTreeEntryByPath(ctx, s.gitRepo, cleaned)
	if err != nil {
		if git.IsErrNotExist(err) {
			return nil, "", unreadableRepo{"There is nothing at " + displayPath(cleaned) + " in " + s.ref + "."}
		}
		return nil, "", err
	}
	return entry, cleaned, nil
}

// isRootPath reports whether a path means the repository itself: nothing, or dots.
func isRootPath(path string) bool {
	return strings.Trim(strings.TrimSpace(path), "./") == ""
}

// displayPath names a path the way a message reads, with the repository root spelled out.
func displayPath(path string) string {
	if path == "" {
		return "the root of the repository"
	}
	return "`" + path + "`"
}

// toldToModel reports whether an error is really an answer: something the model can act on,
// like a path that is not there, rather than a failure of the tool itself.
func toldToModel(err error) (string, bool) {
	if told, ok := errors.AsType[unreadableRepo](err); ok {
		return told.reason, true
	}
	return "", false
}

// listFilesTool lists a directory of the session's repository, so the model can see what is
// there before it reads anything.
type listFilesTool struct{}

func (listFilesTool) Name() string { return "gitea_list_files" }

func (listFilesTool) Description() string {
	return "List the files and directories of one directory of this session's repository. Start " +
		"at the root, then follow the paths it shows. Use it before reading a file you have not seen."
}

func (listFilesTool) Scope() Scope { return ScopeRepository }

// Offered keeps the tool out of a session that has no repository to look at.
func (listFilesTool) Offered(_ context.Context, session Session) (bool, error) {
	return session.RepoID != 0, nil
}

func (listFilesTool) Args() []Arg {
	return []Arg{
		{Name: "path", Optional: true, Description: "Directory to list, relative to the repository root; leave it out for the root"},
		{Name: "ref", Optional: true, Description: "Branch, tag or commit to read; leave it out for the default branch"},
	}
}

func (listFilesTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := listFilesTool{}.list(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (listFilesTool) list(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Path string `json:"path"`
		Ref  string `json:"ref"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	found, err := openSessionRepo(ctx, doer, session, params.Ref)
	if err != nil {
		return "", err
	}
	defer found.gitRepo.Close()

	entry, path, err := found.entryAt(ctx, params.Path)
	if err != nil {
		return "", err
	}
	if !entry.IsDir() {
		return displayPath(path) + " is a file; read it with " + readFileToolName + ".", nil
	}
	tree := entry.Tree(ctx, found.gitRepo)
	if tree == nil {
		return "", errors.New("the directory could not be read")
	}
	entries, err := tree.ListEntries(ctx, found.gitRepo)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return displayPath(path) + " is empty.", nil
	}
	// directories first, then names, so a listing reads the same way every time
	slices.SortStableFunc(entries, func(a, b *git.TreeEntry) int {
		if a.IsDir() != b.IsDir() {
			if a.IsDir() {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name(), b.Name())
	})
	lines := make([]string, 0, len(entries)+1)
	for _, one := range entries {
		name := one.Name()
		if one.IsDir() {
			name += "/"
		}
		lines = append(lines, name)
	}
	if len(lines) > fileListLimit {
		lines = append(lines[:fileListLimit], fmt.Sprintf("… and %d more entries", len(lines)-fileListLimit))
	}
	log.Trace("Agent tool gitea_list_files listed %d entries of %s for user %d", len(entries), found.repository.FullName(), doer.ID)
	return clip(strings.Join(lines, "\n")), nil
}

// readFileToolName is named where another tool answers with what to use instead.
const readFileToolName = "gitea_read_file"

// readFileTool reads one text file of the session's repository.
type readFileTool struct{}

func (readFileTool) Name() string { return readFileToolName }

func (readFileTool) Description() string {
	return "Read one text file of this session's repository, up to " + sizeText(toolOutputLimit) +
		" of it. A picture or another binary file comes back as a line saying so."
}

func (readFileTool) Scope() Scope { return ScopeRepository }

func (readFileTool) Offered(_ context.Context, session Session) (bool, error) {
	return session.RepoID != 0, nil
}

func (readFileTool) Args() []Arg {
	return []Arg{
		{Name: "path", Description: "Path of the file, relative to the repository root, as gitea_list_files shows it"},
		{Name: "ref", Optional: true, Description: "Branch, tag or commit to read; leave it out for the default branch"},
	}
}

func (readFileTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := readFileTool{}.read(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (readFileTool) read(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Path string `json:"path"`
		Ref  string `json:"ref"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	found, err := openSessionRepo(ctx, doer, session, params.Ref)
	if err != nil {
		return "", err
	}
	defer found.gitRepo.Close()

	entry, path, err := found.entryAt(ctx, params.Path)
	if err != nil {
		return "", err
	}
	if entry.IsDir() {
		return displayPath(path) + " is a directory; list it with " + listFilesToolName + ".", nil
	}
	blob := entry.Blob(found.gitRepo)
	size := blob.Size(ctx)
	content, err := blob.GetBlobBytes(ctx, toolOutputLimit)
	if err != nil {
		return "", err
	}
	if !typesniffer.DetectContentType(content).IsText() {
		return fmt.Sprintf("%s is not a text file (%s).", displayPath(path), sizeText(size)), nil
	}
	header := fmt.Sprintf("%s at %s, %s", displayPath(path), found.ref, sizeText(size))
	if int64(len(content)) < size {
		header += ", showing the first " + sizeText(int64(len(content)))
	}
	log.Trace("Agent tool gitea_read_file read %s of %s for user %d", path, found.repository.FullName(), doer.ID)
	return clip(header + "\n\n" + string(content)), nil
}

// listFilesToolName is named where another tool answers with what to use instead.
const listFilesToolName = "gitea_list_files"

// sizeText writes a size the way a reader does, in the units a file is spoken of.
func sizeText(size int64) string {
	const step = 1024
	if size < step {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, suffix := range []string{"KB", "MB", "GB"} {
		value /= step
		if value < step {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f TB", value/step)
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"strings"

	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"

	"xorm.io/builder"
)

// Call modes of a model group.
const (
	// GroupModeFallback tries members in order until one answers.
	GroupModeFallback = "fallback"
	// GroupModeRoundRobin rotates the starting member between calls.
	GroupModeRoundRobin = "round_robin"
)

// Member kinds.
const (
	MemberKindModel = "model"
	MemberKindGroup = "group"
)

// ModelGroup bundles several models behind one name, the way a load balancer fronts
// several backends. Consumers ask for the group and the router picks a member.
type ModelGroup struct {
	ID   int64  `xorm:"pk autoincr"`
	Name string `xorm:"UNIQUE NOT NULL"` // free-form display name
	// Slug is the generated identifier other features refer to. It is created once
	// from the name and stays stable when the name changes.
	Slug        string             `xorm:"UNIQUE NOT NULL"`
	Description string             `xorm:"TEXT"`
	Mode        string             `xorm:"NOT NULL"`           // fallback, round_robin
	Cursor      int                `xorm:"NOT NULL DEFAULT 0"` // round robin position
	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (ModelGroup) TableName() string {
	return "agent_model_group"
}

// ModelGroupMember is one target of a group, in priority order.
type ModelGroupMember struct {
	ID       int64 `xorm:"pk autoincr"`
	GroupID  int64 `xorm:"INDEX NOT NULL"` // the group this member belongs to
	Position int   `xorm:"NOT NULL"`
	// A member is either a single provider model...
	ProviderID int64  `xorm:"NOT NULL DEFAULT 0"`
	Model      string `xorm:"NOT NULL DEFAULT ''"`
	// ...or another group, which makes groups composable.
	MemberGroupID int64              `xorm:"NOT NULL DEFAULT 0"`
	CreatedUnix   timeutil.TimeStamp `xorm:"created NOT NULL"`
}

func (ModelGroupMember) TableName() string {
	return "agent_model_group_member"
}

// Kind reports whether the member is a model or a group.
func (m ModelGroupMember) Kind() string {
	if m.MemberGroupID != 0 {
		return MemberKindGroup
	}
	return MemberKindModel
}

// NormalizeGroupName lowercases and trims a group name; group names are keys, not labels.
func NormalizeGroupName(name string) string {
	return strings.Join(strings.Fields(name), " ")
}

// GroupSlugFromName derives the identifier other features use to reference a group:
// lower case with separators folded to single dashes. It returns "" when the name has
// no readable latin word (e.g. a name made of digits or non-latin script only), so the
// caller generates an identifier instead.
func GroupSlugFromName(name string) string {
	var b strings.Builder
	lastDash := true // never start with a dash
	for _, r := range strings.ToLower(NormalizeGroupName(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 64 {
		slug = strings.Trim(slug[:64], "-")
	}
	hasLetter := strings.IndexFunc(slug, func(r rune) bool { return r >= 'a' && r <= 'z' }) >= 0
	if !hasLetter {
		return ""
	}
	return slug
}

// ModeOrDefault returns the stored mode, falling back to the zero value's meaning.
func (r *ModelGroup) ModeOrDefault() string {
	if r.Mode == GroupModeRoundRobin {
		return GroupModeRoundRobin
	}
	return GroupModeFallback
}

// FindModelGroupsOptions describes filtering options for finding groups.
type FindModelGroupsOptions struct {
	db.ListOptions
	Keyword string // case-insensitive match on the name or description
}

func (opts FindModelGroupsOptions) ToConds() builder.Cond {
	cond := builder.NewCond()
	if opts.Keyword != "" {
		cond = cond.And(builder.Or(
			db.BuildCaseInsensitiveLike("name", opts.Keyword),
			db.BuildCaseInsensitiveLike("slug", opts.Keyword),
			db.BuildCaseInsensitiveLike("description", opts.Keyword),
		))
	}
	return cond
}

func (opts FindModelGroupsOptions) ToOrders() string {
	return "name"
}

// ErrModelGroupNotFound is returned when a group is not found.
type ErrModelGroupNotFound struct {
	ID int64
}

func (err ErrModelGroupNotFound) Error() string {
	return "model group not found"
}

// Is reports whether target is an ErrModelGroupNotFound regardless of the ID.
func (err ErrModelGroupNotFound) Is(target error) bool {
	_, ok := target.(ErrModelGroupNotFound)
	return ok
}

// FindModelGroups returns one page of groups and the total number of matches.
func FindModelGroups(ctx context.Context, opts FindModelGroupsOptions) ([]*ModelGroup, int64, error) {
	return db.FindAndCount[ModelGroup](ctx, opts)
}

// FindGroupMembersOptions describes which group members to read.
type FindGroupMembersOptions struct {
	db.ListOptions
	GroupID int64
	// AllGroups ignores GroupID and returns the members of every group.
	AllGroups bool
}

func (opts FindGroupMembersOptions) ToConds() builder.Cond {
	if opts.AllGroups {
		return builder.NewCond()
	}
	return builder.Eq{"group_id": opts.GroupID}
}

func (opts FindGroupMembersOptions) ToOrders() string {
	return "group_id, position, id"
}

// FindGroupMembers returns the members of a group, in priority order.
func FindGroupMembers(ctx context.Context, groupID int64) ([]*ModelGroupMember, error) {
	return db.Find[ModelGroupMember](ctx, FindGroupMembersOptions{
		ListOptions: db.ListOptionsAll,
		GroupID:     groupID,
	})
}

// FindAllGroupMembers returns the members of every group.
func FindAllGroupMembers(ctx context.Context) ([]*ModelGroupMember, error) {
	return db.Find[ModelGroupMember](ctx, FindGroupMembersOptions{
		ListOptions: db.ListOptionsAll,
		AllGroups:   true,
	})
}

func init() {
	db.RegisterModel(new(ModelGroup))
	db.RegisterModel(new(ModelGroupMember))
}

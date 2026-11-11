// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
)

// ErrModelGroupInvalid is returned when a group can not be stored as requested.
type ErrModelGroupInvalid struct {
	Reason string
}

func (err ErrModelGroupInvalid) Error() string {
	return err.Reason
}

// Is reports whether target is an ErrModelGroupInvalid.
func (err ErrModelGroupInvalid) Is(target error) bool {
	_, ok := target.(ErrModelGroupInvalid)
	return ok
}

// GroupMemberTarget is one requested member of a group: either a provider model or
// another group, which makes groups composable.
type GroupMemberTarget struct {
	ProviderID int64
	Model      string
	GroupID    int64
}

// ModelGroupOptions holds the editable fields of a model group.
type ModelGroupOptions struct {
	// Decisions says the group fronts models that answer the decisions API.
	Decisions   bool
	Name        string
	Description string
	Mode        string
	Members     []GroupMemberTarget
}

// CreateModelGroup stores a new group with its members.
func CreateModelGroup(ctx context.Context, opts ModelGroupOptions) (*agent_model.ModelGroup, error) {
	group, err := buildModelGroup(ctx, 0, opts)
	if err != nil {
		return nil, err
	}
	if err := db.Insert(ctx, group); err != nil {
		return nil, fmt.Errorf("insert model group: %w", err)
	}
	if err := saveGroupMembers(ctx, group.ID, opts.Members); err != nil {
		return nil, err
	}
	return group, nil
}

// UpdateModelGroup replaces the fields and members of an existing group.
func UpdateModelGroup(ctx context.Context, id int64, opts ModelGroupOptions) (*agent_model.ModelGroup, error) {
	existing, err := GetModelGroupByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := validateClassChange(ctx, existing, opts.Decisions); err != nil {
		return nil, err
	}
	group, err := buildModelGroup(ctx, id, opts)
	if err != nil {
		return nil, err
	}

	group.ID = existing.ID
	group.Slug = existing.Slug // the identifier never changes
	group.Cursor = existing.Cursor
	group.Mode = opts.Mode
	if _, err := db.GetEngine(ctx).ID(group.ID).Cols("name", "description", "mode", "updated_unix").Update(group); err != nil {
		return nil, fmt.Errorf("update model group: %w", err)
	}
	if err := saveGroupMembers(ctx, group.ID, opts.Members); err != nil {
		return nil, err
	}
	return group, nil
}

// GetModelGroupByID returns a group by ID.
func GetModelGroupByID(ctx context.Context, id int64) (*agent_model.ModelGroup, error) {
	group, exists, err := db.GetByID[agent_model.ModelGroup](ctx, id)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, agent_model.ErrModelGroupNotFound{ID: id}
	}
	return group, nil
}

// GetModelGroupBySlug returns the group with the given slug, or nil when there is none.
func GetModelGroupBySlug(ctx context.Context, slug string) (*agent_model.ModelGroup, error) {
	group := &agent_model.ModelGroup{}
	exist, err := db.GetEngine(ctx).Where("slug = ?", slug).Get(group)
	if err != nil || !exist {
		return nil, err
	}
	return group, nil
}

// FindModelGroups returns one page of groups and the total number of matches.
func FindModelGroups(ctx context.Context, opts agent_model.FindModelGroupsOptions) ([]*agent_model.ModelGroup, int64, error) {
	return agent_model.FindModelGroups(ctx, opts)
}

// GetGroupMembers returns the members of a group in priority order.
func GetGroupMembers(ctx context.Context, roleID int64) ([]*agent_model.ModelGroupMember, error) {
	return agent_model.FindGroupMembers(ctx, roleID)
}

// GroupMemberCounts returns how many members each group has.
func GroupMemberCounts(ctx context.Context) map[int64]int {
	members, err := agent_model.FindAllGroupMembers(ctx)
	if err != nil {
		return nil
	}
	counts := map[int64]int{}
	for _, m := range members {
		counts[m.GroupID]++
	}
	return counts
}

// DeleteModelGroup removes a group, its members, the memberships of the groups that named it and
// the roles assigned to it, so nothing is left pointing at a group that is gone.
func DeleteModelGroup(ctx context.Context, id int64) error {
	group, err := GetModelGroupByID(ctx, id)
	if err != nil {
		return err
	}
	return db.WithTx(ctx, func(ctx context.Context) error {
		if err := ForgetModelRefs(ctx, ModelRefForGroup(group.Slug)); err != nil {
			return err
		}
		if _, err := db.GetEngine(ctx).Where("member_group_id = ?", id).Delete(&agent_model.ModelGroupMember{}); err != nil {
			return fmt.Errorf("delete memberships of group %d: %w", id, err)
		}
		if _, err := db.Delete[agent_model.ModelGroupMember](ctx, agent_model.FindGroupMembersOptions{
			ListOptions: db.ListOptionsAll,
			GroupID:     id,
		}); err != nil {
			return fmt.Errorf("delete group members: %w", err)
		}
		if _, err := db.DeleteByID[agent_model.ModelGroup](ctx, id); err != nil {
			return fmt.Errorf("delete model group: %w", err)
		}
		return nil
	})
}

// buildModelGroup validates the submitted fields and returns the group to store.
// The identifier is generated here; operators only choose the name.
func buildModelGroup(ctx context.Context, exceptID int64, opts ModelGroupOptions) (*agent_model.ModelGroup, error) {
	name := agent_model.NormalizeGroupName(opts.Name)
	if name == "" {
		return nil, ErrModelGroupInvalid{Reason: "a group name is required"}
	}
	if utf8.RuneCountInString(name) > 100 {
		return nil, ErrModelGroupInvalid{Reason: "the group name is too long"}
	}

	existing, err := modelGroupsByName(ctx)
	if err != nil {
		return nil, err
	}
	slug := agent_model.GroupSlugFromName(name)
	if slug == "" {
		// a name without latin characters still needs an identifier
		slug = generatedSlug()
	}
	for id, taken := range existing {
		if strings.EqualFold(taken.name, name) && id != exceptID {
			return nil, ErrModelGroupInvalid{Reason: fmt.Sprintf("group %q already exists", name)}
		}
		if taken.slug == slug && id != exceptID {
			slug = uniqueSlug(slug, existing, exceptID)
		}
	}

	switch opts.Mode {
	case "", agent_model.GroupModeFallback:
		opts.Mode = agent_model.GroupModeFallback
	case agent_model.GroupModeRoundRobin:
	default:
		return nil, ErrModelGroupInvalid{Reason: fmt.Sprintf("unknown call mode %q", opts.Mode)}
	}

	if len(opts.Members) == 0 {
		return nil, ErrModelGroupInvalid{Reason: "a group needs at least one member"}
	}
	if err := validateMembers(ctx, exceptID, opts.Decisions, opts.Members); err != nil {
		return nil, err
	}

	return &agent_model.ModelGroup{
		Name:        name,
		Slug:        slug,
		Description: opts.Description,
		Mode:        opts.Mode,
		Decisions:   opts.Decisions,
	}, nil
}

// knownGroup is the identifying part of a stored group, used for uniqueness checks.
type knownGroup struct {
	name string
	slug string
}

func modelGroupsByName(ctx context.Context) (map[int64]knownGroup, error) {
	groups, err := db.Find[agent_model.ModelGroup](ctx, agent_model.FindModelGroupsOptions{ListOptions: db.ListOptionsAll})
	if err != nil {
		return nil, err
	}
	known := make(map[int64]knownGroup, len(groups))
	for _, group := range groups {
		known[group.ID] = knownGroup{name: group.Name, slug: group.Slug}
	}
	return known, nil
}

// generatedSlug returns a short opaque identifier for names that yield no slug.
func generatedSlug() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "group"
	}
	return "g-" + hex.EncodeToString(buf)
}

// uniqueSlug appends a counter until the generated identifier is free.
func uniqueSlug(base string, existing map[int64]knownGroup, exceptID int64) string {
	taken := map[string]bool{}
	for id, group := range existing {
		if id != exceptID {
			taken[group.slug] = true
		}
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !taken[candidate] {
			return candidate
		}
	}
}

// modelServesClass reports whether a provider's model answers what a group fronts. A model neither
// listing described is read as one that answers a chat, as it is when it is offered.
func modelServesClass(provider *agent_model.Provider, model string, decisions bool) bool {
	for _, offered := range provider.Models() {
		if offered.ID != model {
			continue
		}
		if decisions {
			return offered.Decisions
		}
		return offered.Chat || !offered.Decisions
	}
	return false
}

// validateClassChange refuses a change of what a group fronts while something expecting the old
// kind still names it: a group that holds it, or a role it is assigned to.
func validateClassChange(ctx context.Context, group *agent_model.ModelGroup, decisions bool) error {
	if group.Decisions == decisions {
		return nil
	}

	parents := make([]*agent_model.ModelGroupMember, 0, 4)
	if err := db.GetEngine(ctx).Where("member_group_id = ?", group.ID).Find(&parents); err != nil {
		return fmt.Errorf("find groups holding group %d: %w", group.ID, err)
	}
	for _, parent := range parents {
		holder, err := GetModelGroupByID(ctx, parent.GroupID)
		if err != nil {
			return err
		}
		if holder.Decisions != decisions {
			return ErrModelGroupInvalid{Reason: fmt.Sprintf("group %q holds this group", holder.Name)}
		}
	}

	ref := ModelRefForGroup(group.Slug)
	roles := make([]*agent_model.ModelRole, 0, len(agent_model.ModelRoleOptions))
	if err := db.GetEngine(ctx).Where("model_ref = ?", ref).Find(&roles); err != nil {
		return fmt.Errorf("find roles using group %d: %w", group.ID, err)
	}
	for _, role := range roles {
		if (agent_model.ModelRoleClassOf(role.Role) == agent_model.ModelRoleClassDecision) != decisions {
			return ErrModelGroupInvalid{Reason: fmt.Sprintf("the %s role answers with this group", role.Role)}
		}
	}
	return nil
}

// validateMembers checks every member points at a real model or group, that it answers what the
// group fronts, and that the group does not end up containing itself.
func validateMembers(ctx context.Context, roleID int64, decisions bool, members []GroupMemberTarget) error {
	for _, member := range members {
		switch {
		case member.GroupID != 0:
			if member.GroupID == roleID {
				return ErrModelGroupInvalid{Reason: "a group can not contain itself"}
			}
			child, err := GetModelGroupByID(ctx, member.GroupID)
			if err != nil {
				if errors.Is(err, agent_model.ErrModelGroupNotFound{}) {
					return ErrModelGroupInvalid{Reason: "a member group does not exist"}
				}
				return err
			}
			if child.Decisions != decisions {
				return ErrModelGroupInvalid{Reason: fmt.Sprintf("group %q fronts something else", child.Name)}
			}
		default:
			if member.Model == "" {
				return ErrModelGroupInvalid{Reason: "a member needs a model"}
			}
			provider, err := GetProviderByID(ctx, member.ProviderID)
			if err != nil {
				if errors.Is(err, agent_model.ErrProviderNotFound{}) {
					return ErrModelGroupInvalid{Reason: "a member provider does not exist"}
				}
				return err
			}
			if !slices.Contains(provider.ModelIDs(), member.Model) {
				return ErrModelGroupInvalid{Reason: fmt.Sprintf("provider %q does not offer model %q", provider.Name, member.Model)}
			}
			if !modelServesClass(provider, member.Model, decisions) {
				return ErrModelGroupInvalid{Reason: fmt.Sprintf("model %q of provider %q does not answer what this group fronts", member.Model, provider.Name)}
			}
		}
	}

	// walking up from each member group must not reach this group
	if roleID != 0 {
		for _, member := range members {
			if member.GroupID == 0 {
				continue
			}
			reaches, err := groupReaches(ctx, member.GroupID, roleID, 0)
			if err != nil {
				return err
			}
			if reaches {
				return ErrModelGroupInvalid{Reason: "a group can not contain itself through another group"}
			}
		}
	}
	return nil
}

// groupReaches reports whether start contains target, following members transitively.
func groupReaches(ctx context.Context, start, target, depth int64) (bool, error) {
	if depth > 16 {
		return false, ErrModelGroupInvalid{Reason: "group nesting is too deep"}
	}
	if start == target {
		return true, nil
	}
	members, err := agent_model.FindGroupMembers(ctx, start)
	if err != nil {
		return false, err
	}
	for _, member := range members {
		if member.MemberGroupID == 0 {
			continue
		}
		reaches, err := groupReaches(ctx, member.MemberGroupID, target, depth+1)
		if err != nil || reaches {
			return reaches, err
		}
	}
	return false, nil
}

// saveGroupMembers replaces the members of a group.
func saveGroupMembers(ctx context.Context, roleID int64, members []GroupMemberTarget) error {
	if _, err := db.Delete[agent_model.ModelGroupMember](ctx, agent_model.FindGroupMembersOptions{
		ListOptions: db.ListOptionsAll,
		GroupID:     roleID,
	}); err != nil {
		return fmt.Errorf("clear group members: %w", err)
	}
	for position, member := range members {
		row := &agent_model.ModelGroupMember{
			GroupID:       roleID,
			Position:      position,
			ProviderID:    member.ProviderID,
			Model:         member.Model,
			MemberGroupID: member.GroupID,
		}
		if row.MemberGroupID != 0 {
			row.ProviderID, row.Model = 0, ""
		}
		if err := db.Insert(ctx, row); err != nil {
			return fmt.Errorf("insert group member: %w", err)
		}
	}
	return nil
}

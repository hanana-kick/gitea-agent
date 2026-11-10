// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"slices"

	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
)

// Model roles are the purposes the agent uses a model for. The admin assigns one model
// to each of them; sessions answer with the model of their kind's role.
const (
	ModelRoleResident   = "resident"
	ModelRoleServer     = "server"
	ModelRoleRepository = "repository"
	ModelRoleSummary    = "summary" // titles and recent work summaries
	// ModelRoleDecision is the model that decides: whether a call may go ahead without a person,
	// and what to do where the agent would otherwise have asked one.
	ModelRoleDecision = "decision"
)

// ModelRoleOptions lists every role in display order.
var ModelRoleOptions = []string{ModelRoleResident, ModelRoleServer, ModelRoleRepository, ModelRoleSummary, ModelRoleDecision}

// What a role needs of its model. A decision role judges: whether a call may go ahead without a
// person, and what to do where the agent would otherwise have asked one. A model role writes: the
// answers, the work and the names. They are different jobs, so an instance may answer them with
// different models.
const (
	// ModelRoleClassDecision is a role that decides.
	ModelRoleClassDecision = "decision"
	// ModelRoleClassModel is a role that writes.
	ModelRoleClassModel = "model"
)

// ModelRoleClasses lists the classes in display order.
var ModelRoleClasses = []string{ModelRoleClassModel, ModelRoleClassDecision}

// ModelRoleClassOf returns what a role needs of its model.
func ModelRoleClassOf(role string) string {
	if role == ModelRoleDecision {
		return ModelRoleClassDecision
	}
	return ModelRoleClassModel
}

// ModelRoleRolesOfClass lists the roles of one class in display order.
func ModelRoleRolesOfClass(class string) []string {
	roles := make([]string, 0, len(ModelRoleOptions))
	for _, role := range ModelRoleOptions {
		if ModelRoleClassOf(role) == class {
			roles = append(roles, role)
		}
	}
	return roles
}

// ModelRoleLightRoles are the roles that carry the small jobs: naming a session, and the like.
// They are answered in seconds from a short prompt, so they are where the cheaper model of an
// instance belongs. Deciding is not a small job, so the decision role is not one of them.
var ModelRoleLightRoles = []string{ModelRoleSummary}

// ModelRoleWorkRoles are the roles that carry the work itself, where the strongest model an
// instance pays for belongs.
var ModelRoleWorkRoles = []string{ModelRoleResident, ModelRoleServer, ModelRoleRepository}

// IsLightModelRole reports whether a role carries a small job rather than the work itself.
func IsLightModelRole(role string) bool {
	return slices.Contains(ModelRoleLightRoles, role)
}

// IsValidModelRole reports whether role is a known model role.
func IsValidModelRole(role string) bool {
	return slices.Contains(ModelRoleOptions, role)
}

// ModelRole assigns a model to one role.
type ModelRole struct {
	ID          int64              `xorm:"pk autoincr"`
	Role        string             `xorm:"UNIQUE NOT NULL"`
	ModelRef    string             `xorm:"VARCHAR(160) NOT NULL DEFAULT ''"` // empty when the role has no model
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (ModelRole) TableName() string {
	return "agent_model_role"
}

func init() {
	db.RegisterModel(new(ModelRole))
}

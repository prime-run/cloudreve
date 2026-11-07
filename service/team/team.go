package team

import (
	"github.com/cloudreve/Cloudreve/v4/application/dependency"
	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/inventory"
	"github.com/cloudreve/Cloudreve/v4/pkg/hashid"
	"github.com/cloudreve/Cloudreve/v4/pkg/serializer"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

// UserTeamResponse is a team membership visible to the current user.
type UserTeamResponse struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	TeamID int    `json:"team_id,omitempty"`
}

// List returns teams the current user belongs to.
func List(c *gin.Context) ([]UserTeamResponse, error) {
	dep := dependency.FromContext(c)
	teamClient := dep.TeamClient()
	hasher := dep.HashIDEncoder()
	user := inventory.UserFromContext(c)

	memberships, err := teamClient.ListByUser(c, user.ID)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to list teams", err)
	}

	return lo.FilterMap(memberships, func(m *ent.TeamMember, _ int) (UserTeamResponse, bool) {
		if m.Edges.Team == nil {
			return UserTeamResponse{}, false
		}
		return UserTeamResponse{
			ID:     hashid.EncodeTeamID(hasher, m.Edges.Team.ID),
			Name:   m.Edges.Team.Name,
			Role:   string(m.Role),
			TeamID: m.Edges.Team.ID,
		}, true
	}), nil
}

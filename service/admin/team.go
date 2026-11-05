package admin

import (
	"context"
	"strings"

	"github.com/cloudreve/Cloudreve/v4/application/dependency"
	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/ent/teammember"
	"github.com/cloudreve/Cloudreve/v4/inventory"
	"github.com/cloudreve/Cloudreve/v4/pkg/hashid"
	"github.com/cloudreve/Cloudreve/v4/pkg/serializer"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

type (
	SingleTeamService struct {
		ID int `uri:"id" json:"id" binding:"required"`
	}
	SingleTeamParamCtx struct{}
)

type (
	CreateTeamService struct {
		Name       string `json:"name" binding:"required,min=1,max=255"`
		MaxStorage *int64 `json:"max_storage"`
	}
	CreateTeamParamCtx struct{}
)

type (
	RenameTeamService struct {
		Name            string `json:"name" binding:"required,min=1,max=255"`
		MaxStorage      *int64 `json:"max_storage"`
		ClearMaxStorage bool   `json:"clear_max_storage"`
	}
	RenameTeamParamCtx struct{}
)

type (
	UpsertTeamMemberService struct {
		UserID string `json:"user_id" binding:"required"`
		Role   string `json:"role" binding:"required"`
	}
	UpsertTeamMemberParamCtx struct{}
)

type (
	RemoveTeamMemberService struct {
		ID     int    `uri:"id" binding:"required"`
		UserID string `uri:"userId" binding:"required"`
	}
	RemoveTeamMemberParamCtx struct{}
)

type TeamMemberResponse struct {
	ID         int       `json:"id"`
	UserID     int       `json:"user_id"`
	UserHashID string    `json:"user_hash_id,omitempty"`
	Email      string    `json:"email,omitempty"`
	Nick       string    `json:"nick,omitempty"`
	Role       string    `json:"role"`
	User       *ent.User `json:"user,omitempty"`
}

type GetTeamResponse struct {
	*ent.Team
	HashID  string               `json:"hash_id"`
	Members []TeamMemberResponse `json:"members,omitempty"`
}

type ListTeamResponse struct {
	Pagination *inventory.PaginationResults `json:"pagination"`
	Teams      []GetTeamResponse            `json:"teams"`
}

func (s *AdminListService) Teams(c *gin.Context) (*ListTeamResponse, error) {
	dep := dependency.FromContext(c)
	teamClient := dep.TeamClient()
	hasher := dep.HashIDEncoder()

	ctx := context.WithValue(c, inventory.LoadTeamMembers{}, true)
	ctx = context.WithValue(ctx, inventory.LoadTeamMemberUser{}, true)

	res, err := teamClient.ListTeams(ctx, &inventory.ListTeamParameters{
		PaginationArgs: &inventory.PaginationArgs{
			Page:     s.Page - 1,
			PageSize: s.PageSize,
			OrderBy:  s.OrderBy,
			Order:    inventory.OrderDirection(s.OrderDirection),
		},
		Name: s.Searches["name"],
	})
	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to list teams", err)
	}

	return &ListTeamResponse{
		Pagination: res.PaginationResults,
		Teams: lo.Map(res.Teams, func(t *ent.Team, _ int) GetTeamResponse {
			return mapTeamResponse(t, hasher)
		}),
	}, nil
}

func (s *SingleTeamService) Get(c *gin.Context) (*GetTeamResponse, error) {
	dep := dependency.FromContext(c)
	teamClient := dep.TeamClient()
	hasher := dep.HashIDEncoder()

	ctx := context.WithValue(c, inventory.LoadTeamMembers{}, true)
	ctx = context.WithValue(ctx, inventory.LoadTeamMemberUser{}, true)
	ctx = context.WithValue(ctx, inventory.LoadTeamRoot{}, true)

	t, err := teamClient.GetByID(ctx, s.ID)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeNotFound, "Team not found", err)
	}

	res := mapTeamResponse(t, hasher)
	return &res, nil
}

func (s *CreateTeamService) Create(c *gin.Context) (*GetTeamResponse, error) {
	dep := dependency.FromContext(c)
	teamClient := dep.TeamClient()
	fileClient := dep.FileClient()
	user := inventory.UserFromContext(c)

	name := strings.TrimSpace(s.Name)
	if name == "" {
		return nil, serializer.NewError(serializer.CodeParamErr, "Name is required", nil)
	}

	fc, tx, ctx, err := inventory.WithTx(c, fileClient)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to start transaction", err)
	}
	tc, _, _, err := inventory.WithTx(ctx, teamClient)
	if err != nil {
		_ = inventory.Rollback(tx)
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to start transaction", err)
	}

	t, err := tc.Create(ctx, name, user.ID, s.MaxStorage, fc)
	if err != nil {
		_ = inventory.Rollback(tx)
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to create team", err)
	}

	if err := inventory.Commit(tx); err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to commit team creation", err)
	}

	service := &SingleTeamService{ID: t.ID}
	return service.Get(c)
}

func (s *SingleTeamService) Rename(c *gin.Context, body *RenameTeamService) (*GetTeamResponse, error) {
	dep := dependency.FromContext(c)
	teamClient := dep.TeamClient()

	name := strings.TrimSpace(body.Name)
	if name == "" {
		return nil, serializer.NewError(serializer.CodeParamErr, "Name is required", nil)
	}

	_, err := teamClient.Update(c, s.ID, name, body.MaxStorage, body.ClearMaxStorage)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to update team", err)
	}

	return s.Get(c)
}

func (s *SingleTeamService) Delete(c *gin.Context) error {
	dep := dependency.FromContext(c)
	teamClient := dep.TeamClient()

	count, err := teamClient.CountRootChildren(c, s.ID)
	if err != nil {
		return serializer.NewError(serializer.CodeDBError, "Failed to check team contents", err)
	}
	if count > 0 {
		return serializer.NewError(serializer.CodeParamErr, "Team root is not empty", nil)
	}

	if err := teamClient.Delete(c, s.ID); err != nil {
		return serializer.NewError(serializer.CodeDBError, "Failed to delete team", err)
	}
	return nil
}

func (s *SingleTeamService) UpsertMember(c *gin.Context, body *UpsertTeamMemberService) (*GetTeamResponse, error) {
	dep := dependency.FromContext(c)
	teamClient := dep.TeamClient()
	userClient := dep.UserClient()
	hasher := dep.HashIDEncoder()

	role, err := parseTeamRole(body.Role)
	if err != nil {
		return nil, err
	}

	userID, err := hasher.Decode(body.UserID, hashid.UserID)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeParamErr, "Invalid user id", err)
	}

	if _, err := userClient.GetByID(c, userID); err != nil {
		return nil, serializer.NewError(serializer.CodeNotFound, "User not found", err)
	}

	if _, err := teamClient.GetByID(c, s.ID); err != nil {
		return nil, serializer.NewError(serializer.CodeNotFound, "Team not found", err)
	}

	_, err = teamClient.UpsertMember(c, s.ID, userID, role)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeParamErr, err.Error(), err)
	}

	return s.Get(c)
}

func (s *RemoveTeamMemberService) Remove(c *gin.Context) (*GetTeamResponse, error) {
	dep := dependency.FromContext(c)
	teamClient := dep.TeamClient()
	hasher := dep.HashIDEncoder()

	userID, err := hasher.Decode(s.UserID, hashid.UserID)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeParamErr, "Invalid user id", err)
	}

	if err := teamClient.RemoveMember(c, s.ID, userID); err != nil {
		if ent.IsNotFound(err) {
			return nil, serializer.NewError(serializer.CodeNotFound, "Member not found", err)
		}
		return nil, serializer.NewError(serializer.CodeParamErr, err.Error(), err)
	}

	service := &SingleTeamService{ID: s.ID}
	return service.Get(c)
}

func parseTeamRole(role string) (teammember.Role, error) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case string(teammember.RoleOwner):
		return teammember.RoleOwner, nil
	case string(teammember.RoleAdmin):
		return teammember.RoleAdmin, nil
	case string(teammember.RoleMember):
		return teammember.RoleMember, nil
	default:
		return "", serializer.NewError(serializer.CodeParamErr, "role must be owner, admin, or member", nil)
	}
}

func mapTeamResponse(t *ent.Team, hasher hashid.Encoder) GetTeamResponse {
	res := GetTeamResponse{
		Team:   t,
		HashID: hashid.EncodeTeamID(hasher, t.ID),
	}
	if t.Edges.Members != nil {
		res.Members = lo.Map(t.Edges.Members, func(m *ent.TeamMember, _ int) TeamMemberResponse {
			mr := TeamMemberResponse{
				ID:     m.ID,
				UserID: m.UserID,
				Role:   string(m.Role),
			}
			if m.Edges.User != nil {
				mr.User = m.Edges.User
				mr.Email = m.Edges.User.Email
				mr.Nick = m.Edges.User.Nick
				mr.UserHashID = hashid.EncodeUserID(hasher, m.Edges.User.ID)
			}
			return mr
		})
	}
	return res
}

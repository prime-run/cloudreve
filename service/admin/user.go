package admin

import (
	"context"
	"strconv"

	"github.com/cloudreve/Cloudreve/v4/application/dependency"
	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/ent/teammember"
	"github.com/cloudreve/Cloudreve/v4/ent/user"
	"github.com/cloudreve/Cloudreve/v4/inventory"
	"github.com/cloudreve/Cloudreve/v4/inventory/types"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/manager"
	"github.com/cloudreve/Cloudreve/v4/pkg/hashid"
	"github.com/cloudreve/Cloudreve/v4/pkg/serializer"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/samber/lo"
)

// AddUserService 用户添加服务
type AddUserService struct {
	//User     model.User `json:"User" binding:"required"`
	Password string `json:"password"`
}

// UserService 用户ID服务
type UserService struct {
	ID uint `uri:"id" json:"id" binding:"required"`
}

// UserBatchService 用户批量操作服务
type UserBatchService struct {
	ID []uint `json:"id" binding:"min=1"`
}

const (
	userStatusCondition = "user_status"
	userGroupCondition  = "user_group"
	userNickCondition   = "user_nick"
	userEmailCondition  = "user_email"
)

func (service *AdminListService) Users(c *gin.Context) (*ListUserResponse, error) {
	dep := dependency.FromContext(c)
	hasher := dep.HashIDEncoder()
	userClient := dep.UserClient()

	ctx := context.WithValue(c, inventory.LoadUserGroup{}, true)
	ctx = context.WithValue(ctx, inventory.LoadUserPasskey{}, true)

	var (
		err     error
		groupID int
	)
	if service.Conditions[userGroupCondition] != "" {
		groupID, err = strconv.Atoi(service.Conditions[userGroupCondition])
		if err != nil {
			return nil, serializer.NewError(serializer.CodeParamErr, "Invalid group ID", err)
		}
	}

	res, err := userClient.ListUsers(ctx, &inventory.ListUserParameters{
		PaginationArgs: &inventory.PaginationArgs{
			Page:     service.Page - 1,
			PageSize: service.PageSize,
			OrderBy:  service.OrderBy,
			Order:    inventory.OrderDirection(service.OrderDirection),
		},
		Status:  user.Status(service.Conditions[userStatusCondition]),
		GroupID: groupID,
		Nick:    service.Conditions[userNickCondition],
		Email:   service.Conditions[userEmailCondition],
	})

	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to list users", err)
	}

	return &ListUserResponse{
		Pagination: res.PaginationResults,
		Users: lo.Map(res.Users, func(user *ent.User, _ int) GetUserResponse {
			return GetUserResponse{
				User:         user,
				HashID:       hashid.EncodeUserID(hasher, user.ID),
				TwoFAEnabled: user.TwoFactorSecret != "",
			}
		}),
	}, nil
}

type (
	SingleUserService struct {
		ID int `uri:"id" json:"id" binding:"required"`
	}
	SingleUserParamCtx struct{}
)

func (service *SingleUserService) Get(c *gin.Context) (*GetUserResponse, error) {
	dep := dependency.FromContext(c)
	hasher := dep.HashIDEncoder()
	userClient := dep.UserClient()
	teamClient := dep.TeamClient()

	ctx := context.WithValue(c, inventory.LoadUserGroup{}, true)
	ctx = context.WithValue(ctx, inventory.LoadUserPasskey{}, true)

	user, err := userClient.GetByID(ctx, service.ID)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to get user", err)
	}

	m := manager.NewFileManager(dep, user)
	capacity, err := m.Capacity(ctx)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeInternalSetting, "Failed to get user capacity", err)
	}

	memberships, err := teamClient.ListByUser(ctx, user.ID)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to list user teams", err)
	}

	teams := lo.FilterMap(memberships, func(m *ent.TeamMember, _ int) (UserTeamMembership, bool) {
		if m.Edges.Team == nil {
			return UserTeamMembership{}, false
		}
		return UserTeamMembership{
			ID:   hashid.EncodeTeamID(hasher, m.Edges.Team.ID),
			Name: m.Edges.Team.Name,
			Role: string(m.Role),
		}, true
	})
	if teams == nil {
		teams = []UserTeamMembership{}
	}

	return &GetUserResponse{
		User:         user,
		HashID:       hashid.EncodeUserID(hasher, user.ID),
		TwoFAEnabled: user.TwoFactorSecret != "",
		Capacity:     capacity,
		Teams:        teams,
	}, nil
}

func (service *SingleUserService) CalibrateStorage(c *gin.Context) (*GetUserResponse, error) {
	dep := dependency.FromContext(c)
	userClient := dep.UserClient()

	ctx := context.WithValue(c, inventory.LoadUserGroup{}, true)
	_, err := userClient.CalculateStorage(ctx, service.ID)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to calculate storage", err)
	}

	subService := &SingleUserService{ID: service.ID}
	return subService.Get(c)
}

type (
	UserTeamAssignment struct {
		ID   string `json:"id" binding:"required"`
		Role string `json:"role" binding:"required"`
	}
	UpsertUserService struct {
		User     *ent.User            `json:"user" binding:"required"`
		Password string               `json:"password" binding:"omitempty,min=6,max=128"`
		TwoFA    string               `json:"two_fa"`
		Teams    []UserTeamAssignment `json:"teams"`
	}
	UpsertUserParamCtx struct{}
)

type adminUserEmailValidation struct {
	Email string `binding:"required,email"`
}

func (s *UpsertUserService) validateEmail() error {
	if s.User == nil {
		return serializer.NewError(serializer.CodeParamErr, "Email format error", nil)
	}
	if err := binding.Validator.ValidateStruct(adminUserEmailValidation{Email: s.User.Email}); err != nil {
		return serializer.NewError(serializer.CodeParamErr, "Email format error", err)
	}
	return nil
}

func (s *UpsertUserService) Update(c *gin.Context) (*GetUserResponse, error) {
	if err := s.validateEmail(); err != nil {
		return nil, err
	}

	dep := dependency.FromContext(c)
	userClient := dep.UserClient()
	teamClient := dep.TeamClient()
	hasher := dep.HashIDEncoder()

	ctx := context.WithValue(c, inventory.LoadUserGroup{}, true)
	existing, err := userClient.GetByID(ctx, s.User.ID)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to get user", err)
	}

	if s.User.ID == 1 && existing.Edges.Group.Permissions.Enabled(int(types.GroupPermissionIsAdmin)) {
		if s.User.GroupUsers != existing.GroupUsers {
			return nil, serializer.NewError(serializer.CodeInvalidActionOnDefaultUser, "Cannot change default user's group", nil)
		}

		if s.User.Status != user.StatusActive {
			return nil, serializer.NewError(serializer.CodeInvalidActionOnDefaultUser, "Cannot change default user's status", nil)
		}

	}

	newUser, err := userClient.Upsert(ctx, s.User, s.Password, s.TwoFA)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to update user", err)
	}

	if s.Teams != nil {
		memberships := make(map[int]teammember.Role, len(s.Teams))
		for _, assignment := range s.Teams {
			teamID, err := hasher.Decode(assignment.ID, hashid.TeamID)
			if err != nil {
				return nil, serializer.NewError(serializer.CodeParamErr, "Invalid team id", err)
			}
			role, err := parseTeamRole(assignment.Role)
			if err != nil {
				return nil, err
			}
			if _, err := teamClient.GetByID(ctx, teamID); err != nil {
				return nil, serializer.NewError(serializer.CodeNotFound, "Team not found", err)
			}
			memberships[teamID] = role
		}
		if err := teamClient.SetUserMemberships(ctx, newUser.ID, memberships); err != nil {
			return nil, serializer.NewError(serializer.CodeParamErr, err.Error(), err)
		}
	}

	service := &SingleUserService{ID: newUser.ID}
	return service.Get(c)
}

func (s *UpsertUserService) Create(c *gin.Context) (*GetUserResponse, error) {
	if err := s.validateEmail(); err != nil {
		return nil, err
	}

	if s.Password == "" {
		return nil, serializer.NewError(serializer.CodeParamErr, "Password is required", nil)
	}

	dep := dependency.FromContext(c)
	userClient := dep.UserClient()

	if s.User.ID != 0 {
		return nil, serializer.NewError(serializer.CodeParamErr, "ID must be 0", nil)
	}

	user, err := userClient.Upsert(c, s.User, s.Password, s.TwoFA)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to create user", err)
	}

	service := &SingleUserService{ID: user.ID}
	return service.Get(c)

}

type (
	BatchUserService struct {
		IDs []int `json:"ids" binding:"min=1"`
	}
	BatchUserParamCtx struct{}
)

func (s *BatchUserService) Delete(c *gin.Context) error {
	dep := dependency.FromContext(c)
	userClient := dep.UserClient()
	fileClient := dep.FileClient()

	current := inventory.UserFromContext(c)
	ae := serializer.NewAggregateError()
	for _, id := range s.IDs {
		if current.ID == id || id == 1 {
			ae.Add(strconv.Itoa(id), serializer.NewError(serializer.CodeInvalidActionOnDefaultUser, "Cannot delete current user", nil))
			continue
		}

		fc, tx, ctx, err := inventory.WithTx(c, fileClient)
		if err != nil {
			ae.Add(strconv.Itoa(id), serializer.NewError(serializer.CodeDBError, "Failed to start transaction", err))
			continue
		}

		uc, _, ctx, err := inventory.WithTx(ctx, userClient)
		if err != nil {
			ae.Add(strconv.Itoa(id), serializer.NewError(serializer.CodeDBError, "Failed to start transaction", err))
			continue
		}

		if err := fc.DeleteByUser(ctx, id); err != nil {
			_ = inventory.Rollback(tx)
			ae.Add(strconv.Itoa(id), serializer.NewError(serializer.CodeDBError, "Failed to delete user files", err))
			continue
		}

		if err := uc.Delete(ctx, id); err != nil {
			_ = inventory.Rollback(tx)
			ae.Add(strconv.Itoa(id), serializer.NewError(serializer.CodeDBError, "Failed to delete user", err))
			continue
		}

		if err := inventory.Commit(tx); err != nil {
			ae.Add(strconv.Itoa(id), serializer.NewError(serializer.CodeDBError, "Failed to commit transaction", err))
			continue
		}
	}

	return ae.Aggregate()
}

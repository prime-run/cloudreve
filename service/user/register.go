package user

import (
	"errors"

	"github.com/cloudreve/Cloudreve/v4/application/dependency"
	"github.com/cloudreve/Cloudreve/v4/ent/user"
	"github.com/cloudreve/Cloudreve/v4/inventory"
	"github.com/cloudreve/Cloudreve/v4/pkg/hashid"
	"github.com/cloudreve/Cloudreve/v4/pkg/serializer"
	"github.com/cloudreve/Cloudreve/v4/pkg/util"
	"github.com/gin-gonic/gin"
)

// RegisterParameterCtx define key fore UserRegisterService
type RegisterParameterCtx struct{}

// UserRegisterService 管理用户注册的服务
type UserRegisterService struct {
	UserName string `form:"username" json:"username" binding:"required,min=5,max=100"`
	Password string `form:"password" json:"password" binding:"required,min=6,max=128"`
	Language string `form:"language" json:"language"`
}

// Register creates an active user. Email activation is disabled; accounts are usable immediately.
func (service *UserRegisterService) Register(c *gin.Context) serializer.Response {
	service.UserName = util.NormalizeUsername(service.UserName)
	if err := util.ValidateUsername(service.UserName); err != nil {
		return serializer.ParamErr(c, err.Error(), err)
	}

	dep := dependency.FromContext(c)
	settings := dep.SettingProvider()

	args := &inventory.NewUserArgs{
		Username:      service.UserName,
		PlainPassword: service.Password,
		Status:        user.StatusActive,
		GroupID:       settings.DefaultGroup(c),
		Language:      service.Language,
	}

	userClient := dep.UserClient()
	uc, tx, _, err := inventory.WithTx(c, userClient)
	if err != nil {
		return serializer.DBErr(c, "Failed to start transaction", err)
	}

	expectedUser, err := uc.Create(c, args)
	if expectedUser != nil {
		util.WithValue(c, inventory.UserCtx{}, expectedUser)
	}

	if err != nil {
		_ = inventory.Rollback(tx)
		if errors.Is(err, inventory.ErrUserUsernameExisted) || errors.Is(err, inventory.ErrUserEmailExisted) {
			return serializer.ErrWithDetails(c, serializer.CodeEmailExisted, "Username already in use", err)
		}
		if errors.Is(err, inventory.ErrInactiveUserExisted) {
			return serializer.ErrWithDetails(c, serializer.CodeEmailExisted, "Username already in use", err)
		}
		return serializer.DBErr(c, "Failed to insert user row", err)
	}

	if err := inventory.Commit(tx); err != nil {
		return serializer.DBErr(c, "Failed to commit user row", err)
	}

	return serializer.Response{Data: BuildUser(expectedUser, dep.HashIDEncoder())}
}

// ActivateUser activates a previously inactive user (legacy links only).
func ActivateUser(c *gin.Context) serializer.Response {
	uid := hashid.FromContext(c)
	dep := dependency.FromContext(c)
	userClient := dep.UserClient()

	inactiveUser, err := userClient.GetByID(c, uid)
	if err != nil {
		return serializer.ErrWithDetails(c, serializer.CodeUserNotFound, "User not fount", err)
	}

	if inactiveUser.Status != user.StatusInactive {
		return serializer.ErrWithDetails(c, serializer.CodeUserCannotActivate, "This user cannot be activated", nil)
	}

	activeUser, err := userClient.SetStatus(c, inactiveUser, user.StatusActive)
	if err != nil {
		return serializer.DBErr(c, "Failed to update user", err)
	}

	util.WithValue(c, inventory.UserCtx{}, activeUser)
	return serializer.Response{Data: BuildUser(activeUser, dep.HashIDEncoder())}
}

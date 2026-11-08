package user

import (
	"context"
	"fmt"

	"github.com/cloudreve/Cloudreve/v4/application/dependency"
	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/ent/user"
	"github.com/cloudreve/Cloudreve/v4/inventory"
	"github.com/cloudreve/Cloudreve/v4/pkg/auth"
	"github.com/cloudreve/Cloudreve/v4/pkg/serializer"
	"github.com/cloudreve/Cloudreve/v4/pkg/util"
	"github.com/gin-gonic/gin"
	"github.com/gofrs/uuid"
	"github.com/pquerna/otp/totp"
)

// LoginParameterCtx define key fore UserLoginService
type LoginParameterCtx struct{}

// UserLoginService 管理用户登录的服务
type UserLoginService struct {
	UserName string `form:"username" json:"username" binding:"required,min=5,max=100"`
	Password string `form:"password" json:"password" binding:"required,min=4,max=128"`
}

// Login 用户登录函数
func (service *UserLoginService) Login(c *gin.Context) (*ent.User, string, error) {
	if err := util.ValidateUsername(service.UserName); err != nil {
		return nil, "", serializer.NewError(serializer.CodeParamErr, err.Error(), err)
	}

	dep := dependency.FromContext(c)
	userClient := dep.UserClient()

	ctx := context.WithValue(c, inventory.LoadUserGroup{}, true)
	expectedUser, err := userClient.GetByUsername(ctx, service.UserName)

	// 一系列校验
	if err != nil {
		err = serializer.NewError(serializer.CodeInvalidPassword, "Incorrect password or username", err)
	} else if checkErr := inventory.CheckPassword(expectedUser, service.Password); checkErr != nil {
		err = serializer.NewError(serializer.CodeInvalidPassword, "Incorrect password or username", err)
	} else if expectedUser.Status == user.StatusManualBanned || expectedUser.Status == user.StatusSysBanned {
		err = serializer.NewError(serializer.CodeUserBaned, "This account has been blocked", nil)
	} else if expectedUser.Status == user.StatusInactive {
		err = serializer.NewError(serializer.CodeUserNotActivated, "This account is not activated", nil)
	}

	if err != nil {
		return nil, "", err
	}

	if expectedUser.TwoFactorSecret != "" {
		twoFaSessionID := uuid.Must(uuid.NewV4())
		dep.KV().Set(fmt.Sprintf("user_2fa_%s", twoFaSessionID), expectedUser.ID, 600)
		return expectedUser, twoFaSessionID.String(), nil
	}

	return expectedUser, "", nil
}

type (
	LoginLogCtx struct{}
)

func IssueToken(c *gin.Context) (*BuiltinLoginResponse, error) {
	dep := dependency.FromContext(c)
	u := inventory.UserFromContext(c)
	token, err := dep.TokenAuth().Issue(c, &auth.IssueTokenArgs{
		User:        u,
		RootTokenID: nil,
	})
	if err != nil {
		return nil, serializer.NewError(serializer.CodeEncryptError, "Failed to issue token pair", err)
	}

	return &BuiltinLoginResponse{
		User:  BuildUser(u, dep.HashIDEncoder()),
		Token: *token,
	}, nil
}

// RefreshTokenParameterCtx define key fore RefreshTokenService
type RefreshTokenParameterCtx struct{}

// RefreshTokenService refresh token service
type RefreshTokenService struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

func (s *RefreshTokenService) Refresh(c *gin.Context) (*auth.Token, error) {
	dep := dependency.FromContext(c)
	token, err := dep.TokenAuth().Refresh(c, s.RefreshToken)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeCredentialInvalid, "Failed to issue token pair", err)
	}

	return token, nil
}

func (s *RefreshTokenService) Delete(c *gin.Context) (string, error) {
	dep := dependency.FromContext(c)
	claims, err := dep.TokenAuth().Claims(c, s.RefreshToken)
	if err != nil {
		return "", serializer.NewError(serializer.CodeCredentialInvalid, "Failed to parse token", err)
	}

	// Block root token
	if claims.RootTokenID != nil {
		tokenSettings := dep.SettingProvider().TokenAuth(c)
		dep.KV().Set(fmt.Sprintf("%s%s", auth.RevokeTokenPrefix, claims.RootTokenID.String()), true, int(tokenSettings.RefreshTokenTTL.Seconds()+10))
	}

	return "", nil
}

type (
	OtpValidationParameterCtx struct{}
	OtpValidationService      struct {
		OTP       string `json:"otp" binding:"required"`
		SessionID string `json:"session_id" binding:"required"`
	}
)

// Verify2FA validates OTP for a pending 2FA login session.
func (service *OtpValidationService) Verify2FA(c *gin.Context) (*ent.User, error) {
	dep := dependency.FromContext(c)
	kv := dep.KV()

	sessionRaw, ok := kv.Get(fmt.Sprintf("user_2fa_%s", service.SessionID))
	if !ok {
		return nil, serializer.NewError(serializer.CodeNotFound, "Session not found", nil)
	}

	uid := sessionRaw.(int)
	ctx := context.WithValue(c, inventory.LoadUserGroup{}, true)
	expectedUser, err := dep.UserClient().GetByID(ctx, uid)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeNotFound, "User not found", err)
	}

	if expectedUser.TwoFactorSecret != "" {
		if !totp.Validate(service.OTP, expectedUser.TwoFactorSecret) {
			err := serializer.NewError(serializer.Code2FACodeErr, "Incorrect 2FA code", nil)
			return nil, err
		}
	}

	kv.Delete("user_2fa_", service.SessionID)
	return expectedUser, nil
}

type (
	PrepareLoginParameterCtx struct{}
	PrepareLoginService      struct {
		Username string `form:"username" binding:"required,min=5,max=100"`
	}
)

func (service *PrepareLoginService) Prepare(c *gin.Context) (*PrepareLoginResponse, error) {
	if err := util.ValidateUsername(service.Username); err != nil {
		return nil, serializer.NewError(serializer.CodeParamErr, err.Error(), err)
	}

	dep := dependency.FromContext(c)
	ctx := context.WithValue(c, inventory.LoadUserPasskey{}, true)
	expectedUser, err := dep.UserClient().GetByUsername(ctx, service.Username)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeNotFound, "User not found", err)
	}

	return &PrepareLoginResponse{
		WebAuthnEnabled: len(expectedUser.Edges.Passkey) > 0,
		PasswordEnabled: expectedUser.Password != "",
	}, nil
}

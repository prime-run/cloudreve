package admin

import (
	"errors"
	"strings"
	"testing"

	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/pkg/serializer"
	"github.com/gin-gonic/gin/binding"
)

func TestUpsertUserServiceValidateUsername(t *testing.T) {
	tests := []struct {
		name     string
		username string
		wantErr  bool
	}{
		{name: "empty", username: "", wantErr: true},
		{name: "too short", username: "abcd", wantErr: true},
		{name: "email-like", username: "admin@test.com", wantErr: true},
		{name: "spaces", username: "user name", wantErr: true},
		{name: "minimum length", username: "admin"},
		{name: "with underscore", username: "user_name"},
		{name: "with hyphen", username: "user-name"},
		{name: "alphanumeric", username: "user123"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &UpsertUserService{User: &ent.User{Username: test.username}}
			err := service.validateUsername()
			if test.wantErr {
				if err == nil {
					t.Fatal("expected username validation to fail")
				}
				var appErr serializer.AppError
				if !errors.As(err, &appErr) {
					t.Fatalf("expected AppError, got %T", err)
				}
				if appErr.Code != serializer.CodeParamErr {
					t.Fatalf("unexpected error code: %d", appErr.Code)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected username validation to pass: %v", err)
			}
		})
	}
}

func TestUpsertUserServiceValidateUsernameRejectsMissingUser(t *testing.T) {
	service := &UpsertUserService{}
	err := service.validateUsername()
	if err == nil {
		t.Fatal("expected missing user validation to fail")
	}

	var appErr serializer.AppError
	if !errors.As(err, &appErr) || appErr.Code != serializer.CodeParamErr {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestUpsertUserServicePasswordBinding(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{name: "empty update", password: ""},
		{name: "short update", password: "12345", wantErr: true},
		{name: "minimum length update", password: "123456"},
		{name: "maximum length", password: strings.Repeat("a", 128)},
		{name: "too long", password: strings.Repeat("a", 129), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &UpsertUserService{
				User:     &ent.User{Username: "admin"},
				Password: test.password,
			}
			err := binding.Validator.ValidateStruct(service)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected password validation to fail")
				}
				return
			}
			if err != nil {
				t.Fatalf("expected password validation to pass: %v", err)
			}
		})
	}
}

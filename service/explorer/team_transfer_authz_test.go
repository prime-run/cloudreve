package explorer

import (
	"testing"

	"github.com/cloudreve/Cloudreve/v4/ent/teammember"
)

func canSendAsRole(role teammember.Role) bool {
	switch role {
	case teammember.RoleOwner, teammember.RoleAdmin, teammember.RoleMember:
		return true
	default:
		return false
	}
}

func TestCanSendAsRole(t *testing.T) {
	if !canSendAsRole(teammember.RoleOwner) || !canSendAsRole(teammember.RoleAdmin) || !canSendAsRole(teammember.RoleMember) {
		t.Fatal("write roles must be allowed to send")
	}
	if canSendAsRole(teammember.RoleViewer) || canSendAsRole("") {
		t.Fatal("viewers must not be allowed to send")
	}
}

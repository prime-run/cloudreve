package admin

import (
	"testing"

	"github.com/cloudreve/Cloudreve/v4/ent/teammember"
)

func TestParseTeamRole(t *testing.T) {
	tests := []struct {
		in      string
		want    teammember.Role
		wantErr bool
	}{
		{in: "owner", want: teammember.RoleOwner},
		{in: "admin", want: teammember.RoleAdmin},
		{in: "member", want: teammember.RoleMember},
		{in: "viewer", want: teammember.RoleViewer},
		{in: " Viewer ", want: teammember.RoleViewer},
		{in: "guest", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, test := range tests {
		got, err := parseTeamRole(test.in)
		if test.wantErr {
			if err == nil {
				t.Fatalf("%q: expected error", test.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", test.in, err)
		}
		if got != test.want {
			t.Fatalf("%q: got %q want %q", test.in, got, test.want)
		}
	}
}

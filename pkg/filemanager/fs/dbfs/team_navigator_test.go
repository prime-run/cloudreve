package dbfs

import (
	"testing"

	"github.com/cloudreve/Cloudreve/v4/ent/teammember"
	"github.com/cloudreve/Cloudreve/v4/pkg/setting"
)

func TestTeamNavigatorCapabilitiesByRole(t *testing.T) {
	n := &teamNavigator{
		config: &setting.DBFS{MaxPageSize: 100},
	}

	writeCaps := []NavigatorCapability{
		NavigatorCapabilityUploadFile,
		NavigatorCapabilityCreateFile,
		NavigatorCapabilityDeleteFile,
		NavigatorCapabilityRenameFile,
		NavigatorCapabilityLockFile,
		NavigatorCapabilityModifyProps,
		NavigatorCapabilityUpdateMetadata,
		NavigatorCapabilityVersionControl,
	}
	readCaps := []NavigatorCapability{
		NavigatorCapabilityDownloadFile,
		NavigatorCapabilityListChildren,
		NavigatorCapabilityEnterFolder,
		NavigatorCapabilityInfo,
		NavigatorCapabilityGenerateThumb,
	}

	assertWriteRole := func(t *testing.T, role teammember.Role) {
		t.Helper()
		n.role = role
		caps := n.Capabilities(false).Capability
		for _, cap := range append(writeCaps, readCaps...) {
			if !caps.Enabled(int(cap)) {
				t.Fatalf("%s missing capability %v", role, cap)
			}
		}
	}

	assertWriteRole(t, teammember.RoleMember)
	assertWriteRole(t, teammember.RoleAdmin)
	assertWriteRole(t, teammember.RoleOwner)

	for _, role := range []teammember.Role{teammember.RoleViewer, ""} {
		n.role = role
		caps := n.Capabilities(false).Capability
		for _, cap := range readCaps {
			if !caps.Enabled(int(cap)) {
				t.Fatalf("%q missing capability %v", role, cap)
			}
		}
		for _, cap := range writeCaps {
			if caps.Enabled(int(cap)) {
				t.Fatalf("%q must not have capability %v", role, cap)
			}
		}
	}
}

func TestTeamNavigatorRefreshCapabilitiesPropagatesToChildren(t *testing.T) {
	n := &teamNavigator{
		config: &setting.DBFS{MaxPageSize: 100},
		role:   teammember.RoleMember,
		root: &File{
			CapabilitiesBs: teamNavigatorCapability,
			Children: map[string]*File{
				"child": {CapabilitiesBs: teamNavigatorCapability},
			},
		},
	}
	n.root.Children["child"].Children = map[string]*File{
		"leaf": {CapabilitiesBs: teamNavigatorCapability},
	}

	n.role = teammember.RoleViewer
	n.refreshCapabilities()

	want := teamViewerNavigatorCapability
	if n.root.CapabilitiesBs != want {
		t.Fatal("root capabilities not refreshed")
	}
	if n.root.Children["child"].CapabilitiesBs != want {
		t.Fatal("child capabilities not refreshed")
	}
	if n.root.Children["child"].Children["leaf"].CapabilitiesBs != want {
		t.Fatal("leaf capabilities not refreshed")
	}
}

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

	n.role = teammember.RoleMember
	memberCaps := n.Capabilities(false).Capability
	for _, cap := range []NavigatorCapability{
		NavigatorCapabilityUploadFile,
		NavigatorCapabilityCreateFile,
		NavigatorCapabilityDeleteFile,
		NavigatorCapabilityLockFile,
		NavigatorCapabilityDownloadFile,
		NavigatorCapabilityListChildren,
	} {
		if !memberCaps.Enabled(int(cap)) {
			t.Fatalf("member missing capability %v", cap)
		}
	}

	n.role = teammember.RoleViewer
	viewerCaps := n.Capabilities(false).Capability
	for _, cap := range []NavigatorCapability{
		NavigatorCapabilityDownloadFile,
		NavigatorCapabilityListChildren,
		NavigatorCapabilityEnterFolder,
		NavigatorCapabilityInfo,
		NavigatorCapabilityGenerateThumb,
	} {
		if !viewerCaps.Enabled(int(cap)) {
			t.Fatalf("viewer missing capability %v", cap)
		}
	}
	for _, cap := range []NavigatorCapability{
		NavigatorCapabilityUploadFile,
		NavigatorCapabilityCreateFile,
		NavigatorCapabilityDeleteFile,
		NavigatorCapabilityRenameFile,
		NavigatorCapabilityLockFile,
		NavigatorCapabilityModifyProps,
		NavigatorCapabilityUpdateMetadata,
		NavigatorCapabilityVersionControl,
	} {
		if viewerCaps.Enabled(int(cap)) {
			t.Fatalf("viewer must not have capability %v", cap)
		}
	}
}

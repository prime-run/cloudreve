package dbfs

import (
	"testing"

	"github.com/cloudreve/Cloudreve/v4/inventory"
	"github.com/cloudreve/Cloudreve/v4/pkg/setting"
)

// Smoke checklist (manual):
// 1. Write-role member of A sends file to B (default RO).
// 2. B lists Incoming/{A}/, downloads; content matches A.
// 3. A updates content; B sees live update.
// 4. RO transfer blocks B mutate; write-through updates A's source.
// 5. Delete stub on B revokes access; viewer on A has no Send action.
// 6. Stub sidebar shows From team / Sent by / Transfer access.

func TestTeamTransferNavigatorCapabilities(t *testing.T) {
	n := &teamTransferNavigator{
		config: &setting.DBFS{MaxPageSize: 100},
	}

	readCaps := []NavigatorCapability{
		NavigatorCapabilityDownloadFile,
		NavigatorCapabilityListChildren,
		NavigatorCapabilityEnterFolder,
		NavigatorCapabilityInfo,
		NavigatorCapabilityGenerateThumb,
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

	// Default / readonly transfer
	n.transfer = &inventory.TeamTransfer{AllowWrite: false}
	caps := n.Capabilities(false).Capability
	for _, cap := range readCaps {
		if !caps.Enabled(int(cap)) {
			t.Fatalf("readonly transfer missing capability %v", cap)
		}
	}
	for _, cap := range writeCaps {
		if caps.Enabled(int(cap)) {
			t.Fatalf("readonly transfer must not have capability %v", cap)
		}
	}

	// Write-through transfer
	n.transfer = &inventory.TeamTransfer{AllowWrite: true}
	caps = n.Capabilities(false).Capability
	for _, cap := range append(writeCaps, readCaps...) {
		if !caps.Enabled(int(cap)) {
			t.Fatalf("write transfer missing capability %v", cap)
		}
	}
}

func TestTeamTransferNavigatorNilTransferIsReadonly(t *testing.T) {
	n := &teamTransferNavigator{
		config: &setting.DBFS{MaxPageSize: 100},
	}
	caps := n.Capabilities(false).Capability
	if caps.Enabled(int(NavigatorCapabilityUploadFile)) {
		t.Fatal("nil transfer must not allow upload")
	}
	if !caps.Enabled(int(NavigatorCapabilityDownloadFile)) {
		t.Fatal("nil transfer should still allow download")
	}
}

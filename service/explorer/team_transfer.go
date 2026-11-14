package explorer

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/cloudreve/Cloudreve/v4/application/constants"
	"github.com/cloudreve/Cloudreve/v4/application/dependency"
	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/ent/teammember"
	"github.com/cloudreve/Cloudreve/v4/inventory"
	"github.com/cloudreve/Cloudreve/v4/inventory/types"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/fs"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/fs/dbfs"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/manager"
	"github.com/cloudreve/Cloudreve/v4/pkg/hashid"
	"github.com/cloudreve/Cloudreve/v4/pkg/serializer"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

const incomingFolderName = "Incoming"

type (
	TeamTransferCreateParamCtx struct{}
	CreateTeamTransferService  struct {
		URIs       []string `json:"uris" binding:"required,min=1"`
		DstTeamID  string   `json:"dst_team_id" binding:"required"`
		AllowWrite bool     `json:"allow_write"`
	}
	TeamTransferResponse struct {
		ID           string `json:"id"`
		DestPath     string `json:"dest_path"`
		SourceURI    string `json:"source_uri"`
		AllowWrite   bool   `json:"allow_write"`
		FromTeam     string `json:"from_team"`
		FromTeamName string `json:"from_team_name"`
	}
)

// Create creates live team-to-team transfer stubs under Incoming/{SourceTeam}/.
func (s *CreateTeamTransferService) Create(c *gin.Context) ([]TeamTransferResponse, error) {
	dep := dependency.FromContext(c)
	user := inventory.UserFromContext(c)
	hasher := dep.HashIDEncoder()
	teamClient := dep.TeamClient()
	transferClient := dep.TeamTransferClient()
	fileClient := dep.FileClient()

	dstTeamID, err := hasher.Decode(s.DstTeamID, hashid.TeamID)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeParamErr, "Invalid destination team id", err)
	}
	ctx := context.WithValue(c.Request.Context(), inventory.LoadTeamRoot{}, true)
	dstTeam, err := teamClient.GetByID(ctx, dstTeamID)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeNotFound, "Destination team not found", err)
	}
	if dstTeam.Edges.Root == nil {
		root, err := fileClient.GetByID(ctx, dstTeam.RootFileID)
		if err != nil {
			return nil, serializer.NewError(serializer.CodeDBError, "Failed to load destination root", err)
		}
		dstTeam.Edges.Root = root
	}

	uris := make([]*fs.URI, 0, len(s.URIs))
	var sourceTeamID int
	for _, raw := range s.URIs {
		uri, err := fs.NewUriFromString(raw)
		if err != nil {
			return nil, serializer.NewError(serializer.CodeParamErr, "Invalid uri", err)
		}
		if uri.FileSystem() != constants.FileSystemTeam {
			return nil, serializer.NewError(serializer.CodeParamErr, "Only team files can be sent", nil)
		}
		teamHash := uri.ID("")
		tid, err := hasher.Decode(teamHash, hashid.TeamID)
		if err != nil {
			return nil, serializer.NewError(serializer.CodeParamErr, "Invalid source team id", err)
		}
		if sourceTeamID == 0 {
			sourceTeamID = tid
		} else if sourceTeamID != tid {
			return nil, serializer.NewError(serializer.CodeParamErr, "All sources must belong to the same team", nil)
		}
		uris = append(uris, uri)
	}
	if sourceTeamID == dstTeamID {
		return nil, serializer.NewError(serializer.CodeParamErr, "Cannot send to the same team", nil)
	}

	membership, err := teamClient.GetMembership(ctx, sourceTeamID, user.ID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, serializer.NewError(serializer.CodeNoPermissionErr, "Not a member of the source team", err)
		}
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to check membership", err)
	}
	switch membership.Role {
	case teammember.RoleOwner, teammember.RoleAdmin, teammember.RoleMember:
	default:
		return nil, serializer.NewError(serializer.CodeNoPermissionErr, "Viewers cannot send files to other teams", nil)
	}

	srcTeam, err := teamClient.GetByID(ctx, sourceTeamID)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeNotFound, "Source team not found", err)
	}

	m := manager.NewFileManager(dep, user)
	defer m.Recycle()

	type srcItem struct {
		uri    *fs.URI
		fileID int
		name   string
		isDir  bool
	}
	items := make([]srcItem, 0, len(uris))
	for _, uri := range uris {
		f, err := m.Get(ctx, uri,
			dbfs.WithRequiredCapabilities(dbfs.NavigatorCapabilityLockFile),
			dbfs.WithFilePublicMetadata(),
			dbfs.WithNotRoot(),
		)
		if err != nil {
			return nil, serializer.NewError(serializer.CodeNotFound, "Failed to get source file", err)
		}
		if f.IsSymbolic() {
			return nil, serializer.NewError(serializer.CodeParamErr, "Cannot send symbolic or already-transferred items", nil)
		}
		items = append(items, srcItem{
			uri:    uri,
			fileID: f.ID(),
			name:   f.Name(),
			isDir:  f.Type() == types.FileTypeFolder,
		})
	}

	incoming, err := ensureChildFolder(ctx, fileClient, dstTeam.Edges.Root, incomingFolderName, dstTeam.Edges.Root.OwnerID)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to create Incoming folder", err)
	}
	fromFolderName := sanitizeFolderName(srcTeam.Name)
	fromFolder, err := ensureChildFolder(ctx, fileClient, incoming, fromFolderName, dstTeam.Edges.Root.OwnerID)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to create source Incoming subfolder", err)
	}

	results := make([]TeamTransferResponse, 0, len(items))
	for _, item := range items {
		fc, tx, txCtx, err := inventory.WithTx(ctx, fileClient)
		if err != nil {
			return nil, serializer.NewError(serializer.CodeDBError, "Failed to start transaction", err)
		}
		tc, _ := inventory.InheritTx(txCtx, transferClient)

		stub, stubName, tr, err := createTransferStubInTx(txCtx, fc, tc, hasher, fromFolder, item.name, item.isDir, &inventory.CreateTeamTransferParams{
			SourceFileID: item.fileID,
			SourceTeamID: sourceTeamID,
			DestTeamID:   dstTeamID,
			CreatedBy:    user.ID,
			AllowWrite:   s.AllowWrite,
		}, map[string]string{
			dbfs.MetadataTransferFromTeam:     hashid.EncodeTeamID(hasher, sourceTeamID),
			dbfs.MetadataTransferFromTeamName: srcTeam.Name,
			dbfs.MetadataTransferBy:           hashid.EncodeUserID(hasher, user.ID),
			dbfs.MetadataTransferByNick:       user.Nick,
			dbfs.MetadataTransferAllowWrite:   lo.Ternary(s.AllowWrite, "1", "0"),
		})
		if err != nil {
			_ = inventory.Rollback(tx)
			return nil, serializer.NewError(serializer.CodeDBError, "Failed to create transfer", err)
		}
		if err := inventory.Commit(tx); err != nil {
			return nil, serializer.NewError(serializer.CodeDBError, "Failed to commit transfer", err)
		}

		destURI := fmt.Sprintf("%s://%s@%s/%s/%s/%s",
			constants.CloudreveScheme,
			hashid.EncodeTeamID(hasher, dstTeamID),
			constants.FileSystemTeam,
			incomingFolderName,
			fromFolderName,
			stubName,
		)
		results = append(results, TeamTransferResponse{
			ID:           hashid.EncodeTeamTransferID(hasher, tr.ID),
			DestPath:     destURI,
			SourceURI:    item.uri.String(),
			AllowWrite:   s.AllowWrite,
			FromTeam:     hashid.EncodeTeamID(hasher, sourceTeamID),
			FromTeamName: srcTeam.Name,
		})
		_ = stub
	}

	return results, nil
}

// createTransferStubInTx creates the stub + transfer row + final redirect in one transaction.
func createTransferStubInTx(
	ctx context.Context,
	fc inventory.FileClient,
	tc inventory.TeamTransferClient,
	hasher hashid.Encoder,
	parent *ent.File,
	baseName string,
	isDir bool,
	params *inventory.CreateTeamTransferParams,
	provenance map[string]string,
) (*ent.File, string, *inventory.TeamTransfer, error) {
	var lastErr error
	for attempt := 0; attempt < 100; attempt++ {
		name := baseName
		if attempt > 0 {
			ext := path.Ext(baseName)
			stem := strings.TrimSuffix(baseName, ext)
			name = fmt.Sprintf("%s (%d)%s", stem, attempt, ext)
		}

		meta := make(map[string]string, len(provenance)+1)
		for k, v := range provenance {
			meta[k] = v
		}
		// Placeholder until we know the transfer id; replaced before commit.
		meta[dbfs.MetadataTeamTransferRedirect] = fmt.Sprintf("%s://pending@%s", constants.CloudreveScheme, constants.FileSystemTeamTransfer)

		var stub *ent.File
		var err error
		if isDir {
			stub, err = fc.CreateFolder(ctx, parent, &inventory.CreateFolderParameters{
				Owner:      parent.OwnerID,
				Name:       name,
				IsSymbolic: true,
				Metadata:   meta,
			})
		} else {
			stub, _, _, err = fc.CreateFile(ctx, parent, &inventory.CreateFileParameters{
				FileType:   types.FileTypeFile,
				Name:       name,
				IsSymbolic: true,
				OwnerID:    parent.OwnerID,
				Metadata:   meta,
			})
		}
		if err != nil {
			if ent.IsConstraintError(err) {
				lastErr = err
				continue
			}
			return nil, "", nil, err
		}

		params.DestFileID = stub.ID
		tr, err := tc.Create(ctx, params)
		if err != nil {
			return nil, "", nil, err
		}

		redirect := fmt.Sprintf("%s://%s@%s", constants.CloudreveScheme, hashid.EncodeTeamTransferID(hasher, tr.ID), constants.FileSystemTeamTransfer)
		if err := fc.UpsertMetadata(ctx, stub, map[string]string{
			dbfs.MetadataTeamTransferRedirect: redirect,
		}, nil); err != nil {
			return nil, "", nil, err
		}
		return stub, name, tr, nil
	}
	return nil, "", nil, fmt.Errorf("could not allocate unique stub name for %q: %w", baseName, lastErr)
}

// DeleteTeamTransfer revokes a transfer by id.
func DeleteTeamTransfer(c *gin.Context) error {
	dep := dependency.FromContext(c)
	user := inventory.UserFromContext(c)
	hasher := dep.HashIDEncoder()
	transferClient := dep.TeamTransferClient()
	teamClient := dep.TeamClient()
	fileClient := dep.FileClient()

	tid, err := hasher.Decode(c.Param("id"), hashid.TeamTransferID)
	if err != nil {
		return serializer.NewError(serializer.CodeParamErr, "Invalid transfer id", err)
	}
	ctx := c.Request.Context()
	tr, err := transferClient.GetByID(ctx, tid)
	if err != nil {
		return serializer.NewError(serializer.CodeNotFound, "Transfer not found", err)
	}

	canRevoke := tr.CreatedBy == user.ID
	if !canRevoke {
		if _, err := teamClient.GetMembership(ctx, tr.DestTeamID, user.ID); err == nil {
			canRevoke = true
		}
	}
	if !canRevoke {
		if m, err := teamClient.GetMembership(ctx, tr.SourceTeamID, user.ID); err == nil {
			switch m.Role {
			case teammember.RoleOwner, teammember.RoleAdmin, teammember.RoleMember:
				canRevoke = true
			}
		}
	}
	if !canRevoke {
		return serializer.NewError(serializer.CodeNoPermissionErr, "No permission to revoke transfer", nil)
	}

	fc, tx, txCtx, err := inventory.WithTx(ctx, fileClient)
	if err != nil {
		return serializer.NewError(serializer.CodeDBError, "Failed to start transaction", err)
	}
	tc, _ := inventory.InheritTx(txCtx, transferClient)

	destFileID := tr.DestFileID
	if err := tc.SoftDelete(txCtx, tr.ID); err != nil {
		_ = inventory.Rollback(tx)
		return serializer.NewError(serializer.CodeDBError, "Failed to revoke transfer", err)
	}
	if destFileID > 0 {
		if stub, err := fc.GetByID(txCtx, destFileID); err == nil {
			if _, _, err := fc.Delete(txCtx, []*ent.File{stub}, nil); err != nil {
				_ = inventory.Rollback(tx)
				return serializer.NewError(serializer.CodeDBError, "Failed to delete transfer stub", err)
			}
		}
	}
	if err := inventory.Commit(tx); err != nil {
		return serializer.NewError(serializer.CodeDBError, "Failed to commit transfer revoke", err)
	}
	return nil
}

func ensureChildFolder(ctx context.Context, fc inventory.FileClient, parent *ent.File, name string, ownerID int) (*ent.File, error) {
	existing, err := fc.GetChildFile(ctx, parent, ownerID, name, true)
	if err == nil {
		return existing, nil
	}
	created, err := fc.CreateFolder(ctx, parent, &inventory.CreateFolderParameters{
		Owner: ownerID,
		Name:  name,
	})
	if err != nil {
		existing, getErr := fc.GetChildFile(ctx, parent, ownerID, name, true)
		if getErr == nil {
			return existing, nil
		}
		return nil, err
	}
	return created, nil
}

func sanitizeFolderName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "Team"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "?", "_", "#", "_")
	name = replacer.Replace(name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "Team"
	}
	return name
}

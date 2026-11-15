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
		DestPath     string `json:"dest_path,omitempty"`
		SourceURI    string `json:"source_uri"`
		AllowWrite   bool   `json:"allow_write"`
		FromTeam     string `json:"from_team"`
		FromTeamName string `json:"from_team_name"`
		Status       string `json:"status"`
	}
	PendingTeamTransferItem struct {
		ID                string `json:"id"`
		SourceFileName    string `json:"source_file_name"`
		SourceIsDir       bool   `json:"source_is_dir"`
		SourceSize        int64  `json:"source_size"`
		SourceTeamID      string `json:"source_team_id"`
		SourceTeamName    string `json:"source_team_name"`
		DestTeamID        string `json:"dest_team_id"`
		DestTeamName      string `json:"dest_team_name"`
		CreatedBy         string `json:"created_by"`
		CreatedByNick     string `json:"created_by_nick"`
		CreatedByUsername string `json:"created_by_username"`
		AllowWrite        bool   `json:"allow_write"`
		CreatedAt         int64  `json:"created_at"`
	}
)

func isSourceWriteRole(role teammember.Role) bool {
	switch role {
	case teammember.RoleOwner, teammember.RoleAdmin, teammember.RoleMember:
		return true
	default:
		return false
	}
}

func isSourceApproverRole(role teammember.Role) bool {
	switch role {
	case teammember.RoleOwner, teammember.RoleAdmin:
		return true
	default:
		return false
	}
}

// Create creates live team-to-team transfer stubs under Incoming/{SourceTeam}/,
// or a pending row when a member sends (owner/admin must confirm).
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
	if !isSourceWriteRole(membership.Role) {
		return nil, serializer.NewError(serializer.CodeNoPermissionErr, "Viewers cannot send files to other teams", nil)
	}
	immediate := isSourceApproverRole(membership.Role)

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

	fromTeam := hashid.EncodeTeamID(hasher, sourceTeamID)
	results := make([]TeamTransferResponse, 0, len(items))

	if !immediate {
		for _, item := range items {
			tr, err := transferClient.Create(ctx, &inventory.CreateTeamTransferParams{
				SourceFileID: item.fileID,
				SourceTeamID: sourceTeamID,
				DestTeamID:   dstTeamID,
				CreatedBy:    user.ID,
				AllowWrite:   s.AllowWrite,
				Status:       inventory.TeamTransferStatusPending,
			})
			if err != nil {
				return nil, serializer.NewError(serializer.CodeDBError, "Failed to create transfer", err)
			}
			results = append(results, TeamTransferResponse{
				ID:           hashid.EncodeTeamTransferID(hasher, tr.ID),
				SourceURI:    item.uri.String(),
				AllowWrite:   s.AllowWrite,
				FromTeam:     fromTeam,
				FromTeamName: srcTeam.Name,
				Status:       inventory.TeamTransferStatusPending,
			})
		}
		return results, nil
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
			Status:       inventory.TeamTransferStatusActive,
		}, map[string]string{
			dbfs.MetadataTransferFromTeam:     fromTeam,
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
			FromTeam:     fromTeam,
			FromTeamName: srcTeam.Name,
			Status:       inventory.TeamTransferStatusActive,
		})
		_ = stub
	}

	return results, nil
}

// ListPendingTeamTransfers returns pending outgoing sends.
// With team_id: that source team only (caller must be owner/admin).
// Without team_id: all pending sends from teams where the caller is owner/admin.
func ListPendingTeamTransfers(c *gin.Context) ([]PendingTeamTransferItem, error) {
	dep := dependency.FromContext(c)
	user := inventory.UserFromContext(c)
	hasher := dep.HashIDEncoder()
	teamClient := dep.TeamClient()
	transferClient := dep.TeamTransferClient()
	fileClient := dep.FileClient()
	ctx := c.Request.Context()

	teamIDs := make([]int, 0)
	teamHash := c.Query("team_id")
	if teamHash != "" {
		sourceTeamID, err := hasher.Decode(teamHash, hashid.TeamID)
		if err != nil {
			return nil, serializer.NewError(serializer.CodeParamErr, "Invalid team id", err)
		}
		membership, err := teamClient.GetMembership(ctx, sourceTeamID, user.ID)
		if err != nil {
			if ent.IsNotFound(err) {
				return nil, serializer.NewError(serializer.CodeNoPermissionErr, "Not a member of the team", err)
			}
			return nil, serializer.NewError(serializer.CodeDBError, "Failed to check membership", err)
		}
		if !isSourceApproverRole(membership.Role) {
			return nil, serializer.NewError(serializer.CodeNoPermissionErr, "Only owners and admins can list pending sends", nil)
		}
		teamIDs = append(teamIDs, sourceTeamID)
	} else {
		memberships, err := teamClient.ListByUser(ctx, user.ID)
		if err != nil {
			return nil, serializer.NewError(serializer.CodeDBError, "Failed to list teams", err)
		}
		for _, m := range memberships {
			if isSourceApproverRole(m.Role) {
				teamIDs = append(teamIDs, m.TeamID)
			}
		}
	}

	out := make([]PendingTeamTransferItem, 0)
	for _, sourceTeamID := range teamIDs {
		pending, err := transferClient.ListPendingBySourceTeam(ctx, sourceTeamID)
		if err != nil {
			return nil, serializer.NewError(serializer.CodeDBError, "Failed to list pending transfers", err)
		}
		srcTeamName := ""
		srcTeamIDHash := hashid.EncodeTeamID(hasher, sourceTeamID)
		if src, err := teamClient.GetByID(ctx, sourceTeamID); err == nil {
			srcTeamName = src.Name
		}
		for _, tr := range pending {
			item := PendingTeamTransferItem{
				ID:             hashid.EncodeTeamTransferID(hasher, tr.ID),
				SourceTeamID:   srcTeamIDHash,
				SourceTeamName: srcTeamName,
				DestTeamID:     hashid.EncodeTeamID(hasher, tr.DestTeamID),
				AllowWrite:     tr.AllowWrite,
				CreatedAt:      tr.CreatedAt.Unix(),
				CreatedBy:      hashid.EncodeUserID(hasher, tr.CreatedBy),
			}
			if tr.SourceFileID > 0 {
				if f, err := fileClient.GetByID(ctx, tr.SourceFileID); err == nil {
					item.SourceFileName = f.Name
					item.SourceIsDir = f.Type == int(types.FileTypeFolder)
					item.SourceSize = f.Size
				}
			}
			if dst, err := teamClient.GetByID(ctx, tr.DestTeamID); err == nil {
				item.DestTeamName = dst.Name
			}
			if u, err := dep.UserClient().GetByID(ctx, tr.CreatedBy); err == nil {
				item.CreatedByNick = u.Nick
				item.CreatedByUsername = u.Username
			}
			out = append(out, item)
		}
	}
	return out, nil
}

// loadSourceFileForConfirm loads the source file for a pending transfer.
// Missing or symbolic sources return dedicated errors; the transfer row itself
// is assumed already loaded without LoadTeamTransferSourceFile.
func loadSourceFileForConfirm(ctx context.Context, fc inventory.FileClient, tr *inventory.TeamTransfer) (*ent.File, error) {
	if tr.SourceFileID == 0 {
		return nil, serializer.NewError(serializer.CodeNotFound, "Source file no longer exists", nil)
	}
	f, err := fc.GetByID(ctx, tr.SourceFileID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, serializer.NewError(serializer.CodeNotFound, "Source file no longer exists", err)
		}
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to load source file", err)
	}
	if f.IsSymbolic {
		return nil, serializer.NewError(serializer.CodeParamErr, "Source file is symbolic", nil)
	}
	return f, nil
}

// confirmPendingTransferInTx creates Incoming/{SourceTeam}, the stub, and activates
// the pending row using the transaction-bound file client. On failure the caller
// must roll back so folders created by this attempt are undone.
func confirmPendingTransferInTx(
	ctx context.Context,
	fc inventory.FileClient,
	tc inventory.TeamTransferClient,
	hasher hashid.Encoder,
	dstRoot *ent.File,
	srcTeamName string,
	srcFile *ent.File,
	tr *inventory.TeamTransfer,
	provenance map[string]string,
) (*ent.File, string, string, error) {
	incoming, err := ensureChildFolder(ctx, fc, dstRoot, incomingFolderName, dstRoot.OwnerID)
	if err != nil {
		return nil, "", "", err
	}
	fromFolderName := sanitizeFolderName(srcTeamName)
	fromFolder, err := ensureChildFolder(ctx, fc, incoming, fromFolderName, dstRoot.OwnerID)
	if err != nil {
		return nil, "", "", err
	}
	isDir := srcFile.Type == int(types.FileTypeFolder)
	stub, stubName, err := createStubForPendingTransferInTx(ctx, fc, tc, hasher, fromFolder, srcFile.Name, isDir, tr, provenance)
	if err != nil {
		return nil, "", "", err
	}
	return stub, stubName, fromFolderName, nil
}

// ConfirmTeamTransfer creates the destination stub for a pending send.
func ConfirmTeamTransfer(c *gin.Context) (*TeamTransferResponse, error) {
	dep := dependency.FromContext(c)
	user := inventory.UserFromContext(c)
	hasher := dep.HashIDEncoder()
	teamClient := dep.TeamClient()
	transferClient := dep.TeamTransferClient()
	fileClient := dep.FileClient()

	tid, err := hasher.Decode(c.Param("id"), hashid.TeamTransferID)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeParamErr, "Invalid transfer id", err)
	}

	ctx := context.WithValue(c.Request.Context(), inventory.LoadTeamRoot{}, true)

	// Load the transfer row first without requiring the source file edge, so a
	// deleted source returns "Source file no longer exists" instead of "Transfer not found".
	tr, err := transferClient.GetByID(ctx, tid)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeNotFound, "Transfer not found", err)
	}
	if tr.Status != inventory.TeamTransferStatusPending {
		return nil, serializer.NewError(serializer.CodeParamErr, "Transfer is not pending", nil)
	}

	membership, err := teamClient.GetMembership(ctx, tr.SourceTeamID, user.ID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, serializer.NewError(serializer.CodeNoPermissionErr, "Not a member of the source team", err)
		}
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to check membership", err)
	}
	if !isSourceApproverRole(membership.Role) {
		return nil, serializer.NewError(serializer.CodeNoPermissionErr, "Only owners and admins can confirm sends", nil)
	}

	srcFile, err := loadSourceFileForConfirm(ctx, fileClient, tr)
	if err != nil {
		return nil, err
	}

	dstTeam, err := teamClient.GetByID(ctx, tr.DestTeamID)
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
	tr.DestTeam = dstTeam

	srcTeam, err := teamClient.GetByID(ctx, tr.SourceTeamID)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeNotFound, "Source team not found", err)
	}
	tr.SourceTeam = srcTeam

	creatorNick := user.Nick
	if u, err := dep.UserClient().GetByID(ctx, tr.CreatedBy); err == nil {
		creatorNick = u.Nick
	}

	fc, tx, txCtx, err := inventory.WithTx(ctx, fileClient)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to start transaction", err)
	}
	tc, _ := inventory.InheritTx(txCtx, transferClient)

	stub, stubName, fromFolderName, err := confirmPendingTransferInTx(txCtx, fc, tc, hasher, dstTeam.Edges.Root, srcTeam.Name, srcFile, tr, map[string]string{
		dbfs.MetadataTransferFromTeam:     hashid.EncodeTeamID(hasher, tr.SourceTeamID),
		dbfs.MetadataTransferFromTeamName: srcTeam.Name,
		dbfs.MetadataTransferBy:           hashid.EncodeUserID(hasher, tr.CreatedBy),
		dbfs.MetadataTransferByNick:       creatorNick,
		dbfs.MetadataTransferAllowWrite:   lo.Ternary(tr.AllowWrite, "1", "0"),
	})
	if err != nil {
		_ = inventory.Rollback(tx)
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to confirm transfer", err)
	}
	if err := inventory.Commit(tx); err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to commit transfer confirm", err)
	}

	destURI := fmt.Sprintf("%s://%s@%s/%s/%s/%s",
		constants.CloudreveScheme,
		hashid.EncodeTeamID(hasher, tr.DestTeamID),
		constants.FileSystemTeam,
		incomingFolderName,
		fromFolderName,
		stubName,
	)
	_ = stub
	return &TeamTransferResponse{
		ID:           hashid.EncodeTeamTransferID(hasher, tr.ID),
		DestPath:     destURI,
		SourceURI:    "",
		AllowWrite:   tr.AllowWrite,
		FromTeam:     hashid.EncodeTeamID(hasher, tr.SourceTeamID),
		FromTeamName: srcTeam.Name,
		Status:       inventory.TeamTransferStatusActive,
	}, nil
}

// createStubForPendingTransferInTx inserts the symbolic stub and activates the existing pending row.
func createStubForPendingTransferInTx(
	ctx context.Context,
	fc inventory.FileClient,
	tc inventory.TeamTransferClient,
	hasher hashid.Encoder,
	parent *ent.File,
	baseName string,
	isDir bool,
	tr *inventory.TeamTransfer,
	provenance map[string]string,
) (*ent.File, string, error) {
	var lastErr error
	for attempt := 0; attempt < 100; attempt++ {
		name := baseName
		if attempt > 0 {
			ext := path.Ext(baseName)
			stem := strings.TrimSuffix(baseName, ext)
			name = fmt.Sprintf("%s (%d)%s", stem, attempt, ext)
		}

		if existing, err := fc.GetChildFile(ctx, parent, parent.OwnerID, name, false); err == nil && existing != nil {
			lastErr = fmt.Errorf("name %q already exists", name)
			continue
		} else if err != nil && !ent.IsNotFound(err) {
			return nil, "", err
		}

		meta := make(map[string]string, len(provenance)+1)
		for k, v := range provenance {
			meta[k] = v
		}
		meta[dbfs.MetadataTeamTransferRedirect] = fmt.Sprintf("%s://pending@%s", constants.CloudreveScheme, constants.FileSystemTeamTransfer)

		if _, err := fc.GetClient().ExecContext(ctx, "SAVEPOINT tt_stub_name"); err != nil {
			return nil, "", err
		}

		fileType := types.FileTypeFile
		if isDir {
			fileType = types.FileTypeFolder
		}
		stub, err := fc.GetClient().File.Create().
			SetOwnerID(parent.OwnerID).
			SetType(int(fileType)).
			SetName(name).
			SetParent(parent).
			SetIsSymbolic(true).
			Save(ctx)
		if err != nil {
			if isNameConflict(err) {
				if _, rbErr := fc.GetClient().ExecContext(ctx, "ROLLBACK TO SAVEPOINT tt_stub_name"); rbErr != nil {
					return nil, "", rbErr
				}
				lastErr = err
				continue
			}
			return nil, "", err
		}
		if err := fc.UpsertMetadata(ctx, stub, meta, nil); err != nil {
			return nil, "", err
		}

		ok, err := tc.Activate(ctx, tr.ID, stub.ID)
		if err != nil {
			return nil, "", err
		}
		if !ok {
			if _, rbErr := fc.GetClient().ExecContext(ctx, "ROLLBACK TO SAVEPOINT tt_stub_name"); rbErr != nil {
				return nil, "", rbErr
			}
			return nil, "", fmt.Errorf("transfer is no longer pending")
		}

		redirect := fmt.Sprintf("%s://%s@%s", constants.CloudreveScheme, hashid.EncodeTeamTransferID(hasher, tr.ID), constants.FileSystemTeamTransfer)
		if err := fc.UpsertMetadata(ctx, stub, map[string]string{
			dbfs.MetadataTeamTransferRedirect: redirect,
		}, nil); err != nil {
			return nil, "", err
		}
		if _, err := fc.GetClient().ExecContext(ctx, "RELEASE SAVEPOINT tt_stub_name"); err != nil {
			return nil, "", err
		}
		return stub, name, nil
	}
	return nil, "", fmt.Errorf("could not allocate unique stub name for %q: %w", baseName, lastErr)
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

		if existing, err := fc.GetChildFile(ctx, parent, parent.OwnerID, name, false); err == nil && existing != nil {
			lastErr = fmt.Errorf("name %q already exists", name)
			continue
		} else if err != nil && !ent.IsNotFound(err) {
			return nil, "", nil, err
		}

		meta := make(map[string]string, len(provenance)+1)
		for k, v := range provenance {
			meta[k] = v
		}
		// Placeholder until we know the transfer id; replaced before commit.
		meta[dbfs.MetadataTeamTransferRedirect] = fmt.Sprintf("%s://pending@%s", constants.CloudreveScheme, constants.FileSystemTeamTransfer)

		// Savepoint so a unique (parent, name) violation does not abort the
		// Postgres transaction; the next attempt can insert "name (n)".
		if _, err := fc.GetClient().ExecContext(ctx, "SAVEPOINT tt_stub_name"); err != nil {
			return nil, "", nil, err
		}

		// Plain insert. CreateFolder's ON CONFLICT returns a raced row, and
		// CreateFile always sets storage_policy_files. Incoming has no policy,
		// so 0 violates files_storage_policies_files. A symbolic stub stores no bytes.
		fileType := types.FileTypeFile
		if isDir {
			fileType = types.FileTypeFolder
		}
		stub, err := fc.GetClient().File.Create().
			SetOwnerID(parent.OwnerID).
			SetType(int(fileType)).
			SetName(name).
			SetParent(parent).
			SetIsSymbolic(true).
			Save(ctx)
		if err != nil {
			if isNameConflict(err) {
				if _, rbErr := fc.GetClient().ExecContext(ctx, "ROLLBACK TO SAVEPOINT tt_stub_name"); rbErr != nil {
					return nil, "", nil, rbErr
				}
				lastErr = err
				continue
			}
			return nil, "", nil, err
		}
		if err := fc.UpsertMetadata(ctx, stub, meta, nil); err != nil {
			return nil, "", nil, err
		}

		params.DestFileID = stub.ID
		if params.Status == "" {
			params.Status = inventory.TeamTransferStatusActive
		}
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
		if _, err := fc.GetClient().ExecContext(ctx, "RELEASE SAVEPOINT tt_stub_name"); err != nil {
			return nil, "", nil, err
		}
		return stub, name, tr, nil
	}
	return nil, "", nil, fmt.Errorf("could not allocate unique stub name for %q: %w", baseName, lastErr)
}

func isNameConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "foreign key") {
		return false
	}
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate")
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
			if isSourceWriteRole(m.Role) {
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
	// File.Delete requires the entities edge; without this flag GetByID leaves it unloaded
	// and the delete error rolls the soft-delete back.
	txCtx = context.WithValue(txCtx, inventory.LoadFileEntity{}, true)
	tc, _ := inventory.InheritTx(txCtx, transferClient)

	destFileID := tr.DestFileID
	if err := tc.SoftDelete(txCtx, tr.ID); err != nil {
		_ = inventory.Rollback(tx)
		return serializer.NewError(serializer.CodeDBError, "Failed to revoke transfer", err)
	}
	if destFileID > 0 {
		stub, err := fc.GetByID(txCtx, destFileID)
		if err != nil {
			if !ent.IsNotFound(err) {
				_ = inventory.Rollback(tx)
				return serializer.NewError(serializer.CodeDBError, "Failed to load transfer stub", err)
			}
		} else if _, _, err := fc.Delete(txCtx, []*ent.File{stub}, nil); err != nil {
			_ = inventory.Rollback(tx)
			return serializer.NewError(serializer.CodeDBError, "Failed to delete transfer stub", err)
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

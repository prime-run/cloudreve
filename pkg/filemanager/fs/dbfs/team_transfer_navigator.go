package dbfs

import (
	"context"
	"fmt"

	"github.com/cloudreve/Cloudreve/v4/application/constants"
	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/inventory"
	"github.com/cloudreve/Cloudreve/v4/inventory/types"
	"github.com/cloudreve/Cloudreve/v4/pkg/boolset"
	"github.com/cloudreve/Cloudreve/v4/pkg/cache"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/fs"
	"github.com/cloudreve/Cloudreve/v4/pkg/hashid"
	"github.com/cloudreve/Cloudreve/v4/pkg/logging"
	"github.com/cloudreve/Cloudreve/v4/pkg/setting"
)

var (
	teamTransferNavigatorCapability      = &boolset.BooleanSet{}
	teamTransferWriteNavigatorCapability = &boolset.BooleanSet{}
)

// NewTeamTransferNavigator creates a navigator for a live team-to-team transfer.
func NewTeamTransferNavigator(u *ent.User, fileClient inventory.FileClient, teamClient inventory.TeamClient,
	transferClient inventory.TeamTransferClient, l logging.Logger, config *setting.DBFS, hasher hashid.Encoder) Navigator {
	n := &teamTransferNavigator{
		user:           u,
		l:              l,
		fileClient:     fileClient,
		teamClient:     teamClient,
		transferClient: transferClient,
		config:         config,
	}
	n.baseNavigator = newBaseNavigator(fileClient, defaultFilter, u, hasher, config)
	return n
}

type (
	teamTransferNavigator struct {
		l              logging.Logger
		user           *ent.User
		fileClient     inventory.FileClient
		teamClient     inventory.TeamClient
		transferClient inventory.TeamTransferClient
		config         *setting.DBFS
		*baseNavigator

		transfer       *inventory.TeamTransfer
		transferRoot   *File
		singleFile     bool
		disableRecycle bool
		persist        func()
	}

	teamTransferNavigatorState struct {
		Transfer     *inventory.TeamTransfer
		TransferRoot *File
		SingleFile   bool
	}
)

func (n *teamTransferNavigator) PersistState(kv cache.Driver, key string) {
	n.disableRecycle = true
	n.persist = func() {
		kv.Set(key, teamTransferNavigatorState{
			Transfer:     n.transfer,
			TransferRoot: n.transferRoot,
			SingleFile:   n.singleFile,
		}, ContextHintTTL)
	}
}

func (n *teamTransferNavigator) RestoreState(s State) error {
	n.disableRecycle = true
	if state, ok := s.(teamTransferNavigatorState); ok {
		n.transfer = state.Transfer
		n.transferRoot = state.TransferRoot
		n.singleFile = state.SingleFile
		return nil
	}
	return fmt.Errorf("invalid state type: %T", s)
}

func (n *teamTransferNavigator) Recycle() {
	if n.persist != nil {
		n.persist()
		n.persist = nil
	}
	if !n.disableRecycle && n.transferRoot != nil {
		n.transferRoot.Recycle()
	}
}

func (n *teamTransferNavigator) ensureTransfer(ctx context.Context, path *fs.URI) error {
	if inventory.IsAnonymousUser(n.user) {
		return ErrLoginRequired
	}

	transferHash := path.ID("")
	if transferHash == "" {
		return fs.ErrPathNotExist.WithError(fmt.Errorf("transfer id required"))
	}
	transferID, err := n.hasher.Decode(transferHash, hashid.TeamTransferID)
	if err != nil {
		return fs.ErrPathNotExist.WithError(fmt.Errorf("invalid transfer id"))
	}

	if n.transfer != nil && n.transfer.ID == transferID && n.transferRoot != nil {
		// Re-validate dest membership on every use.
		if _, err := n.teamClient.GetMembership(ctx, n.transfer.DestTeamID, n.user.ID); err != nil {
			if ent.IsNotFound(err) {
				return ErrPermissionDenied
			}
			return fmt.Errorf("failed to check destination membership: %w", err)
		}
		return nil
	}

	ctx = context.WithValue(ctx, inventory.LoadTeamTransferSourceFile{}, true)
	ctx = context.WithValue(ctx, inventory.LoadTeamTransferSourceTeam{}, true)
	t, err := n.transferClient.GetByID(ctx, transferID)
	if err != nil {
		if ent.IsNotFound(err) {
			return fs.ErrPathNotExist.WithError(err)
		}
		return fmt.Errorf("failed to load team transfer: %w", err)
	}

	if _, err := n.teamClient.GetMembership(ctx, t.DestTeamID, n.user.ID); err != nil {
		if ent.IsNotFound(err) {
			return ErrPermissionDenied
		}
		return fmt.Errorf("failed to check destination membership: %w", err)
	}

	if t.SourceFile == nil {
		return fs.ErrPathNotExist.WithError(fmt.Errorf("source file missing"))
	}
	if t.SourceFile.IsSymbolic {
		return fs.ErrPathNotExist.WithError(fmt.Errorf("source is symbolic"))
	}

	n.transfer = t
	n.transferRoot = newFile(nil, t.SourceFile)
	n.singleFile = t.SourceFile.Type == int(types.FileTypeFile)
	if n.singleFile {
		// Wrap single file under a synthetic parent so children listing matches share behavior.
		parentModel := &ent.File{
			ID:      -t.SourceFile.ID,
			Name:    inventory.RootFolderName,
			Type:    int(types.FileTypeFolder),
			OwnerID: t.SourceFile.OwnerID,
		}
		parent := newFile(nil, parentModel)
		n.transferRoot = newFile(parent, t.SourceFile)
		n.transferRoot.Parent = parent
		parent.Children[t.SourceFile.Name] = n.transferRoot
		parent.Path[pathIndexUser] = path.Root()
		parent.IsUserRoot = true
		parent.OwnerModel = n.user
		parent.CapabilitiesBs = n.Capabilities(false).Capability
		n.transferRoot.OwnerModel = n.user
		n.transferRoot.CapabilitiesBs = parent.CapabilitiesBs
		// Expose folder root as transferRoot for path walking when single file.
		n.transferRoot = parent
	} else {
		n.transferRoot.Path[pathIndexUser] = path.Root()
		n.transferRoot.IsUserRoot = true
		n.transferRoot.OwnerModel = n.user
		n.transferRoot.CapabilitiesBs = n.Capabilities(false).Capability
	}

	rootPath := newTeamTransferUri(hashid.EncodeTeamTransferID(n.hasher, t.ID))
	n.transferRoot.Path[pathIndexRoot] = rootPath
	return nil
}

func (n *teamTransferNavigator) To(ctx context.Context, path *fs.URI) (*File, error) {
	if err := n.ensureTransfer(ctx, path); err != nil {
		return nil, err
	}

	current, lastAncestor := n.transferRoot, n.transferRoot
	elements := path.Elements()

	if n.singleFile {
		if len(elements) == 0 {
			return n.transferRoot, nil
		}
		if len(elements) == 1 {
			child, ok := n.transferRoot.Children[elements[0]]
			if !ok || child.Name() != elements[0] {
				// refresh from DB in case name changed
				src, err := n.fileClient.GetByID(ctx, n.transfer.SourceFileID)
				if err != nil {
					return nil, fs.ErrPathNotExist.WithError(err)
				}
				if src.Name != elements[0] {
					return nil, fs.ErrPathNotExist
				}
				f := newFile(n.transferRoot, src)
				f.OwnerModel = n.user
				f.CapabilitiesBs = n.Capabilities(false).Capability
				return f, nil
			}
			return child, nil
		}
		return nil, fs.ErrPathNotExist
	}

	var err error
	for index, element := range elements {
		lastAncestor = current
		current, err = n.baseNavigator.walkNext(ctx, current, element, index == len(elements)-1)
		if err != nil {
			return lastAncestor, fmt.Errorf("failed to walk into %q: %w", element, err)
		}
		current.OwnerModel = n.user
		current.CapabilitiesBs = n.Capabilities(false).Capability
	}
	return current, nil
}

func (n *teamTransferNavigator) Children(ctx context.Context, parent *File, args *ListArgs) (*ListResult, error) {
	if n.singleFile && parent == n.transferRoot {
		src, err := n.fileClient.GetByID(ctx, n.transfer.SourceFileID)
		if err != nil {
			return nil, err
		}
		f := newFile(parent, src)
		f.OwnerModel = n.user
		f.CapabilitiesBs = n.Capabilities(false).Capability
		return &ListResult{
			Files:          []*File{f},
			Pagination:     &inventory.PaginationResults{},
			SingleFileView: true,
		}, nil
	}
	return n.baseNavigator.children(ctx, parent, args)
}

func (n *teamTransferNavigator) Capabilities(isSearching bool) *fs.NavigatorProps {
	caps := teamTransferNavigatorCapability
	if n.transfer != nil && n.transfer.AllowWrite {
		caps = teamTransferWriteNavigatorCapability
	}
	res := &fs.NavigatorProps{
		Capability:            caps,
		OrderDirectionOptions: fullOrderDirectionOption,
		OrderByOptions:        fullOrderByOption,
		MaxPageSize:           n.config.MaxPageSize,
	}
	if isSearching {
		res.OrderByOptions = nil
		res.OrderDirectionOptions = nil
	}
	return res
}

func (n *teamTransferNavigator) FollowTx(ctx context.Context) (func(), error) {
	if _, ok := ctx.Value(inventory.TxCtx{}).(*inventory.Tx); !ok {
		return nil, fmt.Errorf("navigator: no inherited transaction found in context")
	}
	newFileClient, _, _, err := inventory.WithTx(ctx, n.fileClient)
	if err != nil {
		return nil, err
	}
	newTeamClient, _, _, err := inventory.WithTx(ctx, n.teamClient)
	if err != nil {
		return nil, err
	}
	newTransferClient, _, _, err := inventory.WithTx(ctx, n.transferClient)
	if err != nil {
		return nil, err
	}

	oldFile, oldTeam, oldTransfer := n.fileClient, n.teamClient, n.transferClient
	revert := func() {
		n.fileClient = oldFile
		n.teamClient = oldTeam
		n.transferClient = oldTransfer
		n.baseNavigator.fileClient = oldFile
	}
	n.fileClient = newFileClient
	n.teamClient = newTeamClient
	n.transferClient = newTransferClient
	n.baseNavigator.fileClient = newFileClient
	return revert, nil
}

func (n *teamTransferNavigator) ExecuteHook(ctx context.Context, hookType fs.HookType, file *File) error {
	return nil
}

func (n *teamTransferNavigator) Walk(ctx context.Context, levelFiles []*File, limit, depth int, f WalkFunc) error {
	return n.baseNavigator.walk(ctx, levelFiles, limit, depth, f)
}

func (n *teamTransferNavigator) GetView(ctx context.Context, file *File) *types.ExplorerView {
	return file.View()
}

func (n *teamTransferNavigator) SourceTeamID() int {
	if n.transfer == nil {
		return 0
	}
	return n.transfer.SourceTeamID
}

func newTeamTransferUri(transferHash string) *fs.URI {
	res, _ := fs.NewUriFromString(fmt.Sprintf("%s://%s@%s",
		constants.CloudreveScheme, transferHash, constants.FileSystemTeamTransfer))
	return res
}

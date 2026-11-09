package dbfs

import (
	"context"
	"fmt"

	"github.com/cloudreve/Cloudreve/v4/application/constants"
	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/ent/teammember"
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
	teamNavigatorCapability       = &boolset.BooleanSet{}
	teamViewerNavigatorCapability = &boolset.BooleanSet{}
)

// NewTeamNavigator creates a navigator for a team's shared file space.
func NewTeamNavigator(u *ent.User, fileClient inventory.FileClient, teamClient inventory.TeamClient,
	l logging.Logger, config *setting.DBFS, hasher hashid.Encoder) Navigator {
	return &teamNavigator{
		user:          u,
		l:             l,
		fileClient:    fileClient,
		teamClient:    teamClient,
		config:        config,
		baseNavigator: newBaseNavigator(fileClient, defaultFilter, u, hasher, config),
	}
}

type teamNavigator struct {
	l          logging.Logger
	user       *ent.User
	fileClient inventory.FileClient
	teamClient inventory.TeamClient
	config     *setting.DBFS
	*baseNavigator

	root           *File
	team           *ent.Team
	role           teammember.Role
	disableRecycle bool
	persist        func()
}

type teamNavigatorState struct {
	Root *File
	Team *ent.Team
}

func (n *teamNavigator) Recycle() {
	if n.persist != nil {
		n.persist()
		n.persist = nil
	}
	if n.root != nil && !n.disableRecycle {
		n.root.Recycle()
	}
}

func (n *teamNavigator) PersistState(kv cache.Driver, key string) {
	n.disableRecycle = true
	n.persist = func() {
		kv.Set(key, teamNavigatorState{Root: n.root, Team: n.team}, ContextHintTTL)
	}
}

func (n *teamNavigator) RestoreState(s State) error {
	n.disableRecycle = true
	if state, ok := s.(teamNavigatorState); ok {
		n.root = state.Root
		n.team = state.Team
		return nil
	}
	return fmt.Errorf("invalid state type: %T", s)
}

// ensureMembership loads (and refreshes) the caller's role for the team in path.
// Called from getNavigator before capability checks, and from To on every walk.
func (n *teamNavigator) ensureMembership(ctx context.Context, path *fs.URI) (int, error) {
	if inventory.IsAnonymousUser(n.user) {
		return 0, ErrLoginRequired
	}

	teamHash := path.ID("")
	if teamHash == "" {
		return 0, fs.ErrPathNotExist.WithError(fmt.Errorf("team id required"))
	}

	teamID, err := n.hasher.Decode(teamHash, hashid.TeamID)
	if err != nil {
		return 0, fs.ErrPathNotExist.WithError(fmt.Errorf("invalid team id"))
	}

	m, err := n.teamClient.GetMembership(ctx, teamID, n.user.ID)
	if err != nil {
		if ent.IsNotFound(err) {
			n.root = nil
			n.team = nil
			n.role = ""
			return 0, ErrPermissionDenied
		}
		return 0, fmt.Errorf("failed to get team membership: %w", err)
	}
	n.role = m.Role
	return teamID, nil
}

func (n *teamNavigator) To(ctx context.Context, path *fs.URI) (*File, error) {
	teamID, err := n.ensureMembership(ctx, path)
	if err != nil {
		return nil, err
	}

	if n.root == nil || n.team == nil || n.team.ID != teamID {
		ctx = context.WithValue(ctx, inventory.LoadTeamRoot{}, true)
		t, err := n.teamClient.GetByID(ctx, teamID)
		if err != nil {
			return nil, fs.ErrPathNotExist.WithError(fmt.Errorf("team not found: %w", err))
		}
		if t.Edges.Root == nil {
			rootFile, err := n.fileClient.GetByID(ctx, t.RootFileID)
			if err != nil {
				return nil, fs.ErrPathNotExist.WithError(fmt.Errorf("team root not found: %w", err))
			}
			t.Edges.Root = rootFile
		}

		n.team = t
		n.root = newFile(nil, t.Edges.Root)
		rootPath := newTeamIDUri(hashid.EncodeTeamID(n.hasher, t.ID))
		n.root.Path[pathIndexRoot], n.root.Path[pathIndexUser] = rootPath, rootPath
		// Use requesting user as OwnerModel so storage policy / capacity resolve to the member.
		n.root.OwnerModel = n.user
		n.root.IsUserRoot = true
	}
	// Keep file-level caps aligned with the latest membership role.
	if n.root != nil {
		n.root.CapabilitiesBs = n.Capabilities(false).Capability
	}

	current, lastAncestor := n.root, n.root
	elements := path.Elements()
	for index, element := range elements {
		lastAncestor = current
		current, err = n.walkNext(ctx, current, element, index == len(elements)-1)
		if err != nil {
			return lastAncestor, fmt.Errorf("failed to walk into %q: %w", element, err)
		}
	}

	return current, nil
}

func (n *teamNavigator) Children(ctx context.Context, parent *File, args *ListArgs) (*ListResult, error) {
	return n.baseNavigator.children(ctx, parent, args)
}

func (n *teamNavigator) walkNext(ctx context.Context, root *File, next string, isLeaf bool) (*File, error) {
	return n.baseNavigator.walkNext(ctx, root, next, isLeaf)
}

func (n *teamNavigator) Capabilities(isSearching bool) *fs.NavigatorProps {
	caps := teamNavigatorCapability
	if n.role == teammember.RoleViewer {
		caps = teamViewerNavigatorCapability
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

func (n *teamNavigator) Walk(ctx context.Context, levelFiles []*File, limit, depth int, f WalkFunc) error {
	return n.baseNavigator.walk(ctx, levelFiles, limit, depth, f)
}

func (n *teamNavigator) FollowTx(ctx context.Context) (func(), error) {
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

	oldFileClient, oldTeamClient := n.fileClient, n.teamClient
	revert := func() {
		n.fileClient = oldFileClient
		n.teamClient = oldTeamClient
		n.baseNavigator.fileClient = oldFileClient
	}

	n.fileClient = newFileClient
	n.teamClient = newTeamClient
	n.baseNavigator.fileClient = newFileClient
	return revert, nil
}

func (n *teamNavigator) ExecuteHook(ctx context.Context, hookType fs.HookType, file *File) error {
	return nil
}

func (n *teamNavigator) GetView(ctx context.Context, file *File) *types.ExplorerView {
	return file.View()
}

func newTeamIDUri(teamHash string) *fs.URI {
	res, _ := fs.NewUriFromString(fmt.Sprintf("%s://%s@%s",
		constants.CloudreveScheme, teamHash, constants.FileSystemTeam))
	return res
}

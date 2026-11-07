package inventory

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/ent/file"
	"github.com/cloudreve/Cloudreve/v4/ent/schema"
	"github.com/cloudreve/Cloudreve/v4/ent/team"
	"github.com/cloudreve/Cloudreve/v4/ent/teammember"
	"github.com/cloudreve/Cloudreve/v4/inventory/types"
	"github.com/cloudreve/Cloudreve/v4/pkg/conf"
)

type (
	LoadTeamMembers    struct{}
	LoadTeamRoot       struct{}
	LoadTeamMemberUser struct{}
	// TeamStorageIDCtx marks the current transaction / capacity check as accounting
	// against a team shared quota instead of the uploading user's personal storage.
	TeamStorageIDCtx struct{}
)

type (
	TeamClient interface {
		TxOperator
		// Create creates a team with a root folder owned by ownerID and seeds owner membership.
		Create(ctx context.Context, name string, ownerID int, maxStorage *int64, fileClient FileClient) (*ent.Team, error)
		// GetByID returns a team by id.
		GetByID(ctx context.Context, id int) (*ent.Team, error)
		// GetByRootFileID returns the team whose root folder is rootFileID.
		GetByRootFileID(ctx context.Context, rootFileID int) (*ent.Team, error)
		// ListTeams returns paginated teams.
		ListTeams(ctx context.Context, args *ListTeamParameters) (*ListTeamResult, error)
		// Update updates a team's name and/or max_storage.
		Update(ctx context.Context, id int, name string, maxStorage *int64, clearMaxStorage bool) (*ent.Team, error)
		// Rename updates a team's name.
		Rename(ctx context.Context, id int, name string) (*ent.Team, error)
		// Delete hard-deletes a team, its members, and the empty root folder in one transaction.
		// Caller must ensure root is empty.
		Delete(ctx context.Context, id int) error
		// CountRootChildren returns number of direct children under the team root folder.
		CountRootChildren(ctx context.Context, teamID int) (int, error)
		// ListByUser returns teams the user belongs to, with membership edges loaded.
		ListByUser(ctx context.Context, userID int) ([]*ent.TeamMember, error)
		// GetMembership returns membership for a user in a team.
		GetMembership(ctx context.Context, teamID, userID int) (*ent.TeamMember, error)
		// UpsertMember adds or updates a member role.
		UpsertMember(ctx context.Context, teamID, userID int, role teammember.Role) (*ent.TeamMember, error)
		// RemoveMember removes a member. Refuses if removing the last owner.
		RemoveMember(ctx context.Context, teamID, userID int) error
		// CountOwners returns the number of owners in a team.
		CountOwners(ctx context.Context, teamID int) (int, error)
		// SetUserMemberships replaces the user's team memberships with the given set.
		// teams maps teamID -> role. Teams not listed are removed (except last-owner guard).
		SetUserMemberships(ctx context.Context, userID int, memberships map[int]teammember.Role) error
		// ReserveStorage atomically adds size bytes to team teamID's storage while
		// enforcing an optional quota (maxTotal <= 0 means unlimited).
		ReserveStorage(ctx context.Context, teamID int, size int64, maxTotal int64) error
		// ApplyStorageDiff adds diff bytes to team teamID's storage (may be negative).
		ApplyStorageDiff(ctx context.Context, teamID int, diff int64) error
		// CalculateStorage recalculates used storage under the team root and updates team.storage.
		CalculateStorage(ctx context.Context, teamID int) (int64, error)
	}

	ListTeamParameters struct {
		*PaginationArgs
		Name string
	}

	ListTeamResult struct {
		*PaginationResults
		Teams []*ent.Team
	}
)

func NewTeamClient(client *ent.Client, dbType conf.DBType) TeamClient {
	return &teamClient{client: client, maxSQlParam: sqlParamLimit(dbType)}
}

type teamClient struct {
	client      *ent.Client
	maxSQlParam int
}

func (c *teamClient) SetClient(newClient *ent.Client) TxOperator {
	return &teamClient{client: newClient, maxSQlParam: c.maxSQlParam}
}

func (c *teamClient) GetClient() *ent.Client {
	return c.client
}

func (c *teamClient) Create(ctx context.Context, name string, ownerID int, maxStorage *int64, fileClient FileClient) (*ent.Team, error) {
	fc, _ := InheritTx(ctx, fileClient)
	root, err := fc.CreateFolder(ctx, nil, &CreateFolderParameters{
		Owner: ownerID,
		Name:  TeamRootFolderName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create team root folder: %w", err)
	}

	create := c.client.Team.Create().
		SetName(name).
		SetRootFileID(root.ID)
	if maxStorage != nil {
		create.SetMaxStorage(*maxStorage)
	}
	t, err := create.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create team: %w", err)
	}

	_, err = c.client.TeamMember.Create().
		SetTeamID(t.ID).
		SetUserID(ownerID).
		SetRole(teammember.RoleOwner).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to seed team owner membership: %w", err)
	}

	return c.GetByID(ctx, t.ID)
}

func (c *teamClient) GetByID(ctx context.Context, id int) (*ent.Team, error) {
	return withTeamEagerLoading(ctx, c.client.Team.Query().Where(team.ID(id))).First(ctx)
}

func (c *teamClient) GetByRootFileID(ctx context.Context, rootFileID int) (*ent.Team, error) {
	return withTeamEagerLoading(ctx, c.client.Team.Query().Where(team.RootFileID(rootFileID))).First(ctx)
}

func (c *teamClient) ListTeams(ctx context.Context, args *ListTeamParameters) (*ListTeamResult, error) {
	query := withTeamEagerLoading(ctx, c.client.Team.Query())
	if args.Name != "" {
		query = query.Where(team.NameContainsFold(args.Name))
	}

	pageSize := capPageSize(c.maxSQlParam, args.PageSize, 10)
	queryWithoutOrder := query.Clone()
	query.Order(getTeamOrderOption(args)...)

	total, err := queryWithoutOrder.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	teams, err := query.Clone().
		Limit(pageSize).
		Offset(args.Page * pageSize).
		All(ctx)
	if err != nil {
		return nil, err
	}

	return &ListTeamResult{
		Teams: teams,
		PaginationResults: &PaginationResults{
			TotalItems: total,
			Page:       args.Page,
			PageSize:   pageSize,
		},
	}, nil
}

func (c *teamClient) Update(ctx context.Context, id int, name string, maxStorage *int64, clearMaxStorage bool) (*ent.Team, error) {
	upd := c.client.Team.UpdateOneID(id)
	if name != "" {
		upd.SetName(name)
	}
	if clearMaxStorage {
		upd.ClearMaxStorage()
	} else if maxStorage != nil {
		upd.SetMaxStorage(*maxStorage)
	}
	t, err := upd.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to update team: %w", err)
	}
	return c.GetByID(ctx, t.ID)
}

func (c *teamClient) Rename(ctx context.Context, id int, name string) (*ent.Team, error) {
	return c.Update(ctx, id, name, nil, false)
}

func (c *teamClient) Delete(ctx context.Context, id int) error {
	tc, tx, ctx, err := WithTx(ctx, c)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}

	t, err := tc.client.Team.Query().Where(team.ID(id)).Only(ctx)
	if err != nil {
		_ = Rollback(tx)
		return fmt.Errorf("failed to get team: %w", err)
	}
	rootID := t.RootFileID

	// Hard-delete so the root_file_id FK can be cleared before removing the root folder.
	hardCtx := schema.SkipSoftDelete(ctx)
	if _, err := tc.client.TeamMember.Delete().Where(teammember.TeamID(id)).Exec(hardCtx); err != nil {
		_ = Rollback(tx)
		return fmt.Errorf("failed to delete team members: %w", err)
	}
	if err := tc.client.Team.DeleteOneID(id).Exec(hardCtx); err != nil {
		_ = Rollback(tx)
		return fmt.Errorf("failed to delete team: %w", err)
	}
	if _, err := tc.client.File.Delete().Where(file.IDEQ(rootID)).Exec(ctx); err != nil {
		_ = Rollback(tx)
		return fmt.Errorf("failed to delete team root folder: %w", err)
	}

	if err := Commit(tx); err != nil {
		return fmt.Errorf("failed to commit team delete: %w", err)
	}
	return nil
}

func (c *teamClient) CountRootChildren(ctx context.Context, teamID int) (int, error) {
	t, err := c.client.Team.Query().Where(team.ID(teamID)).WithRoot().Only(ctx)
	if err != nil {
		return 0, err
	}
	if t.Edges.Root == nil {
		return 0, fmt.Errorf("team root not loaded")
	}
	return t.Edges.Root.QueryChildren().Count(ctx)
}

func (c *teamClient) ListByUser(ctx context.Context, userID int) ([]*ent.TeamMember, error) {
	q := c.client.TeamMember.Query().
		Where(teammember.UserID(userID)).
		WithTeam()
	return q.All(ctx)
}

func (c *teamClient) GetMembership(ctx context.Context, teamID, userID int) (*ent.TeamMember, error) {
	return withTeamMemberEagerLoading(ctx, c.client.TeamMember.Query().Where(
		teammember.TeamID(teamID),
		teammember.UserID(userID),
	)).First(ctx)
}

func (c *teamClient) lockTeam(ctx context.Context, teamID int) error {
	_, err := c.client.Team.Update().
		Where(team.ID(teamID)).
		SetUpdatedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("failed to lock team: %w", err)
	}
	return nil
}

func (c *teamClient) UpsertMember(ctx context.Context, teamID, userID int, role teammember.Role) (*ent.TeamMember, error) {
	tc, tx, ctx, err := WithTx(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("failed to start transaction: %w", err)
	}

	if err := tc.lockTeam(ctx, teamID); err != nil {
		_ = Rollback(tx)
		return nil, err
	}

	existing, err := tc.client.TeamMember.Query().Where(
		teammember.TeamID(teamID),
		teammember.UserID(userID),
	).Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		_ = Rollback(tx)
		return nil, err
	}

	var member *ent.TeamMember
	if existing != nil {
		if existing.Role == teammember.RoleOwner && role != teammember.RoleOwner {
			owners, err := tc.CountOwners(ctx, teamID)
			if err != nil {
				_ = Rollback(tx)
				return nil, err
			}
			if owners <= 1 {
				_ = Rollback(tx)
				return nil, fmt.Errorf("cannot demote the last owner")
			}
		}
		member, err = tc.client.TeamMember.UpdateOne(existing).SetRole(role).Save(ctx)
		if err != nil {
			_ = Rollback(tx)
			return nil, err
		}
	} else {
		member, err = tc.client.TeamMember.Create().
			SetTeamID(teamID).
			SetUserID(userID).
			SetRole(role).
			Save(ctx)
		if err != nil {
			_ = Rollback(tx)
			return nil, err
		}
	}

	if err := Commit(tx); err != nil {
		return nil, fmt.Errorf("failed to commit member upsert: %w", err)
	}
	return member, nil
}

func (c *teamClient) RemoveMember(ctx context.Context, teamID, userID int) error {
	tc, tx, ctx, err := WithTx(ctx, c)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}

	if err := tc.lockTeam(ctx, teamID); err != nil {
		_ = Rollback(tx)
		return err
	}

	m, err := tc.client.TeamMember.Query().Where(
		teammember.TeamID(teamID),
		teammember.UserID(userID),
	).Only(ctx)
	if err != nil {
		_ = Rollback(tx)
		return err
	}

	if m.Role == teammember.RoleOwner {
		owners, err := tc.CountOwners(ctx, teamID)
		if err != nil {
			_ = Rollback(tx)
			return err
		}
		if owners <= 1 {
			_ = Rollback(tx)
			return fmt.Errorf("cannot remove the last owner")
		}
	}

	if err := tc.client.TeamMember.DeleteOne(m).Exec(ctx); err != nil {
		_ = Rollback(tx)
		return err
	}
	if err := Commit(tx); err != nil {
		return fmt.Errorf("failed to commit member remove: %w", err)
	}
	return nil
}

func (c *teamClient) CountOwners(ctx context.Context, teamID int) (int, error) {
	return c.client.TeamMember.Query().Where(
		teammember.TeamID(teamID),
		teammember.RoleEQ(teammember.RoleOwner),
	).Count(ctx)
}

func (c *teamClient) SetUserMemberships(ctx context.Context, userID int, memberships map[int]teammember.Role) error {
	tc, tx, ctx, err := WithTx(ctx, c)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}

	existing, err := tc.client.TeamMember.Query().Where(teammember.UserID(userID)).All(ctx)
	if err != nil {
		_ = Rollback(tx)
		return err
	}

	existingByTeam := make(map[int]*ent.TeamMember, len(existing))
	affectedTeams := make(map[int]struct{}, len(existing)+len(memberships))
	for _, m := range existing {
		existingByTeam[m.TeamID] = m
		affectedTeams[m.TeamID] = struct{}{}
	}
	for teamID := range memberships {
		affectedTeams[teamID] = struct{}{}
	}

	teamIDs := make([]int, 0, len(affectedTeams))
	for id := range affectedTeams {
		teamIDs = append(teamIDs, id)
	}
	// Stable lock order so overlapping SetUserMemberships cannot deadlock.
	sort.Ints(teamIDs)

	// Touch team rows to take write locks and serialize concurrent last-owner demotions.
	for _, id := range teamIDs {
		if err := tc.lockTeam(ctx, id); err != nil {
			_ = Rollback(tx)
			return err
		}
	}

	// Upsert owner roles first so demotions cannot briefly leave a team ownerless.
	for teamID, role := range memberships {
		if role != teammember.RoleOwner {
			continue
		}
		if _, err := tc.UpsertMember(ctx, teamID, userID, role); err != nil {
			_ = Rollback(tx)
			return err
		}
	}
	for teamID, role := range memberships {
		if role == teammember.RoleOwner {
			continue
		}
		if _, err := tc.UpsertMember(ctx, teamID, userID, role); err != nil {
			_ = Rollback(tx)
			return err
		}
	}

	for teamID := range existingByTeam {
		if _, keep := memberships[teamID]; keep {
			continue
		}
		if err := tc.RemoveMember(ctx, teamID, userID); err != nil {
			_ = Rollback(tx)
			return err
		}
	}

	// Final guard: every touched team that still has members must retain an owner.
	for teamID := range affectedTeams {
		members, err := tc.client.TeamMember.Query().Where(teammember.TeamID(teamID)).Count(ctx)
		if err != nil {
			_ = Rollback(tx)
			return err
		}
		if members == 0 {
			continue
		}
		owners, err := tc.CountOwners(ctx, teamID)
		if err != nil {
			_ = Rollback(tx)
			return err
		}
		if owners < 1 {
			_ = Rollback(tx)
			return fmt.Errorf("cannot leave team %d without an owner", teamID)
		}
	}

	if err := Commit(tx); err != nil {
		return fmt.Errorf("failed to commit membership changes: %w", err)
	}
	return nil
}

func (c *teamClient) ReserveStorage(ctx context.Context, teamID int, size int64, maxTotal int64) error {
	if size == 0 {
		return nil
	}
	if size < 0 {
		return c.ApplyStorageDiff(ctx, teamID, size)
	}

	q := c.client.Team.Update().Where(team.ID(teamID))
	if maxTotal > 0 {
		q = q.Where(team.StorageLTE(maxTotal - size))
	}
	n, err := q.AddStorage(size).Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrInsufficientCapacity
	}
	return nil
}

func (c *teamClient) ApplyStorageDiff(ctx context.Context, teamID int, diff int64) error {
	if diff == 0 {
		return nil
	}
	return runWithDeadlockRetry(func() error {
		return c.client.Team.Update().Where(team.ID(teamID)).AddStorage(diff).Exec(ctx)
	})
}

func (c *teamClient) CalculateStorage(ctx context.Context, teamID int) (int64, error) {
	t, err := c.client.Team.Query().Where(team.ID(teamID)).Only(ctx)
	if err != nil {
		return 0, err
	}

	sum, err := c.sumSubtreeEntitySize(ctx, t.RootFileID)
	if err != nil {
		return 0, err
	}

	if _, err := c.client.Team.UpdateOneID(teamID).SetStorage(sum).Save(ctx); err != nil {
		return 0, err
	}
	return sum, nil
}

// sumSubtreeEntitySize walks the folder tree under rootID and sums entity sizes
// for every file (same basis as user CalculateStorage).
func (c *teamClient) sumSubtreeEntitySize(ctx context.Context, rootID int) (int64, error) {
	queue := []int{rootID}
	var sum int64
	batchSize := 500

	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]

		offset := 0
		for {
			children, err := c.client.File.Query().
				Where(file.FileChildren(parentID)).
				WithEntities().
				Offset(offset).
				Limit(batchSize).
				All(ctx)
			if err != nil {
				return 0, fmt.Errorf("failed to list team files: %w", err)
			}
			if len(children) == 0 {
				break
			}
			for _, child := range children {
				if child.Type == int(types.FileTypeFolder) {
					queue = append(queue, child.ID)
					continue
				}
				for _, e := range child.Edges.Entities {
					sum += e.Size
				}
			}
			if len(children) < batchSize {
				break
			}
			offset += batchSize
		}
	}
	return sum, nil
}

// SumStorageDiff returns the total of all values in a StorageDiff.
func SumStorageDiff(diffs StorageDiff) int64 {
	var sum int64
	for _, v := range diffs {
		sum += v
	}
	return sum
}

// TeamStorageIDFromCtx returns the team ID used for shared storage accounting, if any.
func TeamStorageIDFromCtx(ctx context.Context) (int, bool) {
	id, ok := ctx.Value(TeamStorageIDCtx{}).(int)
	return id, ok && id > 0
}

func getTeamOrderOption(args *ListTeamParameters) []team.OrderOption {
	orderTerm := getOrderTerm(args.Order)
	switch args.OrderBy {
	case team.FieldName:
		return []team.OrderOption{team.ByName(orderTerm), team.ByID(orderTerm)}
	case team.FieldCreatedAt:
		return []team.OrderOption{team.ByCreatedAt(orderTerm), team.ByID(orderTerm)}
	default:
		return []team.OrderOption{team.ByID(orderTerm)}
	}
}

func withTeamEagerLoading(ctx context.Context, q *ent.TeamQuery) *ent.TeamQuery {
	if _, ok := ctx.Value(LoadTeamMembers{}).(bool); ok {
		q.WithMembers(func(tmq *ent.TeamMemberQuery) {
			withTeamMemberEagerLoading(ctx, tmq)
		})
	}
	if _, ok := ctx.Value(LoadTeamRoot{}).(bool); ok {
		q.WithRoot()
	}
	return q
}

func withTeamMemberEagerLoading(ctx context.Context, q *ent.TeamMemberQuery) *ent.TeamMemberQuery {
	if _, ok := ctx.Value(LoadTeamMemberUser{}).(bool); ok {
		q.WithUser()
	}
	if _, ok := ctx.Value(LoadTeamRoot{}).(bool); ok {
		q.WithTeam(func(tq *ent.TeamQuery) {
			tq.WithRoot()
		})
	}
	return q
}

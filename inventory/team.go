package inventory

import (
	"context"
	"fmt"

	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/ent/team"
	"github.com/cloudreve/Cloudreve/v4/ent/teammember"
	"github.com/cloudreve/Cloudreve/v4/pkg/conf"
)

type (
	LoadTeamMembers struct{}
	LoadTeamRoot    struct{}
	LoadTeamMemberUser struct{}
)

type (
	TeamClient interface {
		TxOperator
		// Create creates a team with a root folder owned by ownerID and seeds owner membership.
		Create(ctx context.Context, name string, ownerID int, fileClient FileClient) (*ent.Team, error)
		// GetByID returns a team by id.
		GetByID(ctx context.Context, id int) (*ent.Team, error)
		// ListTeams returns paginated teams.
		ListTeams(ctx context.Context, args *ListTeamParameters) (*ListTeamResult, error)
		// Rename updates a team's name.
		Rename(ctx context.Context, id int, name string) (*ent.Team, error)
		// Delete deletes a team (soft-delete). Caller must ensure root is empty.
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

func (c *teamClient) Create(ctx context.Context, name string, ownerID int, fileClient FileClient) (*ent.Team, error) {
	fc, _ := InheritTx(ctx, fileClient)
	root, err := fc.CreateFolder(ctx, nil, &CreateFolderParameters{
		Owner: ownerID,
		Name:  TeamRootFolderName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create team root folder: %w", err)
	}

	t, err := c.client.Team.Create().
		SetName(name).
		SetRootFileID(root.ID).
		Save(ctx)
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

func (c *teamClient) Rename(ctx context.Context, id int, name string) (*ent.Team, error) {
	t, err := c.client.Team.UpdateOneID(id).SetName(name).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to rename team: %w", err)
	}
	return c.GetByID(ctx, t.ID)
}

func (c *teamClient) Delete(ctx context.Context, id int) error {
	// Soft-delete members first, then the team.
	if _, err := c.client.TeamMember.Delete().Where(teammember.TeamID(id)).Exec(ctx); err != nil {
		return fmt.Errorf("failed to delete team members: %w", err)
	}
	if err := c.client.Team.DeleteOneID(id).Exec(ctx); err != nil {
		return fmt.Errorf("failed to delete team: %w", err)
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

func (c *teamClient) UpsertMember(ctx context.Context, teamID, userID int, role teammember.Role) (*ent.TeamMember, error) {
	existing, err := c.client.TeamMember.Query().Where(
		teammember.TeamID(teamID),
		teammember.UserID(userID),
	).Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return nil, err
	}

	if existing != nil {
		if existing.Role == teammember.RoleOwner && role != teammember.RoleOwner {
			owners, err := c.CountOwners(ctx, teamID)
			if err != nil {
				return nil, err
			}
			if owners <= 1 {
				return nil, fmt.Errorf("cannot demote the last owner")
			}
		}
		return c.client.TeamMember.UpdateOne(existing).SetRole(role).Save(ctx)
	}

	return c.client.TeamMember.Create().
		SetTeamID(teamID).
		SetUserID(userID).
		SetRole(role).
		Save(ctx)
}

func (c *teamClient) RemoveMember(ctx context.Context, teamID, userID int) error {
	m, err := c.client.TeamMember.Query().Where(
		teammember.TeamID(teamID),
		teammember.UserID(userID),
	).Only(ctx)
	if err != nil {
		return err
	}

	if m.Role == teammember.RoleOwner {
		owners, err := c.CountOwners(ctx, teamID)
		if err != nil {
			return err
		}
		if owners <= 1 {
			return fmt.Errorf("cannot remove the last owner")
		}
	}

	return c.client.TeamMember.DeleteOne(m).Exec(ctx)
}

func (c *teamClient) CountOwners(ctx context.Context, teamID int) (int, error) {
	return c.client.TeamMember.Query().Where(
		teammember.TeamID(teamID),
		teammember.RoleEQ(teammember.RoleOwner),
	).Count(ctx)
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

package inventory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/ent/team"
	"github.com/cloudreve/Cloudreve/v4/pkg/conf"
)

// TeamTransfer is a live link of a source team file into a destination team.
// Stored via migrate schema; CRUD uses database/sql through the ent client
// (ent codegen for this entity is not available on the current toolchain).
type TeamTransfer struct {
	ID           int
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    *time.Time
	AllowWrite   bool
	SourceFileID int // 0 when cleared after source delete / revoke
	SourceTeamID int
	DestTeamID   int
	DestFileID   int // 0 when cleared after stub delete / revoke
	CreatedBy    int

	SourceFile *ent.File
	SourceTeam *ent.Team
	DestTeam   *ent.Team
	Creator    *ent.User
}

type (
	LoadTeamTransferSourceFile struct{}
	LoadTeamTransferSourceTeam struct{}
	LoadTeamTransferDestTeam   struct{}
	LoadTeamTransferCreator    struct{}
)

type CreateTeamTransferParams struct {
	SourceFileID int
	SourceTeamID int
	DestTeamID   int
	DestFileID   int
	CreatedBy    int
	AllowWrite   bool
}

type TeamTransferClient interface {
	TxOperator
	Create(ctx context.Context, args *CreateTeamTransferParams) (*TeamTransfer, error)
	GetByID(ctx context.Context, id int) (*TeamTransfer, error)
	GetByDestFileID(ctx context.Context, destFileID int) (*TeamTransfer, error)
	// SoftDelete revokes a transfer and nulls FK columns so file hard-delete can proceed.
	SoftDelete(ctx context.Context, id int) error
	SoftDeleteByDestFileIDs(ctx context.Context, destFileIDs []int) error
	SoftDeleteBySourceFileIDs(ctx context.Context, sourceFileIDs []int) error
	DirectoryTeams(ctx context.Context) ([]*ent.Team, error)
}

func NewTeamTransferClient(client *ent.Client, dbType conf.DBType) TeamTransferClient {
	return &teamTransferClient{client: client, dbType: dbType, maxSQlParam: sqlParamLimit(dbType)}
}

type teamTransferClient struct {
	client      *ent.Client
	dbType      conf.DBType
	maxSQlParam int
}

func (c *teamTransferClient) SetClient(newClient *ent.Client) TxOperator {
	return &teamTransferClient{client: newClient, dbType: c.dbType, maxSQlParam: c.maxSQlParam}
}

func (c *teamTransferClient) GetClient() *ent.Client {
	return c.client
}

func (c *teamTransferClient) ph(n int) string {
	if c.dbType == conf.PostgresDB {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf("$%d", i+1)
		}
		return strings.Join(parts, ", ")
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ", ")
}

func (c *teamTransferClient) Create(ctx context.Context, args *CreateTeamTransferParams) (*TeamTransfer, error) {
	now := time.Now()
	ph := c.ph(8)
	_, err := c.client.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO team_transfers
  (created_at, updated_at, allow_write, source_file_id, source_team_id, dest_team_id, dest_file_id, created_by)
VALUES (%s)
`, ph), now, now, args.AllowWrite, args.SourceFileID, args.SourceTeamID, args.DestTeamID, args.DestFileID, args.CreatedBy)
	if err != nil {
		return nil, fmt.Errorf("failed to create team transfer: %w", err)
	}
	return c.GetByDestFileID(ctx, args.DestFileID)
}

func (c *teamTransferClient) GetByID(ctx context.Context, id int) (*TeamTransfer, error) {
	ph := c.ph(1)
	rows, err := c.client.QueryContext(ctx, fmt.Sprintf(`
SELECT id, created_at, updated_at, deleted_at, allow_write, source_file_id, source_team_id, dest_team_id, dest_file_id, created_by
FROM team_transfers
WHERE id = %s AND deleted_at IS NULL
`, ph), id)
	if err != nil {
		return nil, fmt.Errorf("failed to get team transfer: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, &ent.NotFoundError{}
	}
	t, err := scanTeamTransfer(rows)
	if err != nil {
		return nil, err
	}
	if err := c.eagerLoad(ctx, t); err != nil {
		return nil, err
	}
	return t, rows.Err()
}

func (c *teamTransferClient) GetByDestFileID(ctx context.Context, destFileID int) (*TeamTransfer, error) {
	ph := c.ph(1)
	rows, err := c.client.QueryContext(ctx, fmt.Sprintf(`
SELECT id, created_at, updated_at, deleted_at, allow_write, source_file_id, source_team_id, dest_team_id, dest_file_id, created_by
FROM team_transfers
WHERE dest_file_id = %s AND deleted_at IS NULL
`, ph), destFileID)
	if err != nil {
		return nil, fmt.Errorf("failed to get team transfer by dest file: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, &ent.NotFoundError{}
	}
	t, err := scanTeamTransfer(rows)
	if err != nil {
		return nil, err
	}
	if err := c.eagerLoad(ctx, t); err != nil {
		return nil, err
	}
	return t, rows.Err()
}

func (c *teamTransferClient) SoftDelete(ctx context.Context, id int) error {
	now := time.Now()
	parts := strings.Split(c.ph(3), ", ")
	_, err := c.client.ExecContext(ctx, fmt.Sprintf(`
UPDATE team_transfers
SET deleted_at = %s, updated_at = %s, source_file_id = NULL, dest_file_id = NULL
WHERE id = %s AND deleted_at IS NULL
`, parts[0], parts[1], parts[2]), now, now, id)
	if err != nil {
		return fmt.Errorf("failed to delete team transfer: %w", err)
	}
	return nil
}

func (c *teamTransferClient) SoftDeleteByDestFileIDs(ctx context.Context, destFileIDs []int) error {
	return c.softDeleteByFileIDs(ctx, "dest_file_id", destFileIDs)
}

func (c *teamTransferClient) SoftDeleteBySourceFileIDs(ctx context.Context, sourceFileIDs []int) error {
	return c.softDeleteByFileIDs(ctx, "source_file_id", sourceFileIDs)
}

func (c *teamTransferClient) softDeleteByFileIDs(ctx context.Context, column string, ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	now := time.Now()
	// Batch to stay under max SQL params (2 timestamps + N ids).
	batchSize := c.maxSQlParam - 2
	if batchSize < 1 {
		batchSize = 1
	}
	for start := 0; start < len(ids); start += batchSize {
		end := start + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		ph := c.ph(2 + len(chunk))
		parts := strings.Split(ph, ", ")
		inList := strings.Join(parts[2:], ", ")
		args := make([]any, 0, 2+len(chunk))
		args = append(args, now, now)
		for _, id := range chunk {
			args = append(args, id)
		}
		_, err := c.client.ExecContext(ctx, fmt.Sprintf(`
UPDATE team_transfers
SET deleted_at = %s, updated_at = %s, source_file_id = NULL, dest_file_id = NULL
WHERE %s IN (%s) AND deleted_at IS NULL
`, parts[0], parts[1], column, inList), args...)
		if err != nil {
			return fmt.Errorf("failed to delete team transfers by %s: %w", column, err)
		}
	}
	return nil
}

func (c *teamTransferClient) DirectoryTeams(ctx context.Context) ([]*ent.Team, error) {
	return c.client.Team.Query().Order(ent.Asc(team.FieldName)).All(ctx)
}

func (c *teamTransferClient) eagerLoad(ctx context.Context, t *TeamTransfer) error {
	if _, ok := ctx.Value(LoadTeamTransferSourceFile{}).(bool); ok {
		if t.SourceFileID == 0 {
			return &ent.NotFoundError{}
		}
		f, err := c.client.File.Get(ctx, t.SourceFileID)
		if err != nil {
			if ent.IsNotFound(err) {
				return err
			}
			return fmt.Errorf("failed to load source file: %w", err)
		}
		t.SourceFile = f
	}
	if _, ok := ctx.Value(LoadTeamTransferSourceTeam{}).(bool); ok {
		team, err := c.client.Team.Get(ctx, t.SourceTeamID)
		if err != nil {
			return fmt.Errorf("failed to load source team: %w", err)
		}
		t.SourceTeam = team
	}
	if _, ok := ctx.Value(LoadTeamTransferDestTeam{}).(bool); ok {
		team, err := c.client.Team.Get(ctx, t.DestTeamID)
		if err != nil {
			return fmt.Errorf("failed to load dest team: %w", err)
		}
		t.DestTeam = team
	}
	if _, ok := ctx.Value(LoadTeamTransferCreator{}).(bool); ok {
		u, err := c.client.User.Get(ctx, t.CreatedBy)
		if err != nil {
			return fmt.Errorf("failed to load creator: %w", err)
		}
		t.Creator = u
	}
	return nil
}

func scanTeamTransfer(rows *sql.Rows) (*TeamTransfer, error) {
	var (
		t            TeamTransfer
		deletedAt    sql.NullTime
		sourceFileID sql.NullInt64
		destFileID   sql.NullInt64
	)
	if err := rows.Scan(
		&t.ID, &t.CreatedAt, &t.UpdatedAt, &deletedAt, &t.AllowWrite,
		&sourceFileID, &t.SourceTeamID, &t.DestTeamID, &destFileID, &t.CreatedBy,
	); err != nil {
		return nil, fmt.Errorf("failed to scan team transfer: %w", err)
	}
	if deletedAt.Valid {
		t.DeletedAt = &deletedAt.Time
	}
	if sourceFileID.Valid {
		t.SourceFileID = int(sourceFileID.Int64)
	}
	if destFileID.Valid {
		t.DestFileID = int(destFileID.Int64)
	}
	return &t, nil
}

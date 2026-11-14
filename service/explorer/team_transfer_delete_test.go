package explorer

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/ent/enttest"
	"github.com/cloudreve/Cloudreve/v4/inventory"
	"github.com/cloudreve/Cloudreve/v4/inventory/types"
	"github.com/cloudreve/Cloudreve/v4/pkg/boolset"
	"github.com/cloudreve/Cloudreve/v4/pkg/conf"
	"github.com/cloudreve/Cloudreve/v4/pkg/hashid"
	"github.com/stretchr/testify/require"
)

func openTransferTestDB(t *testing.T) *ent.Client {
	t.Helper()
	client := enttest.Open(t, "sqlite3", filepath.Join(t.TempDir(), "transfer.db")+"?_fk=1&_time_format=sqlite")
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	// team_transfers is created by the migrate schema (with FKs). Ensure it exists
	// when the generated schema already applied it.
	_, err := client.ExecContext(context.Background(), `
CREATE TABLE IF NOT EXISTS team_transfers (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  created_at datetime NOT NULL,
  updated_at datetime NOT NULL,
  deleted_at datetime,
  allow_write bool NOT NULL DEFAULT 0,
  source_file_id INTEGER,
  source_team_id INTEGER NOT NULL,
  dest_team_id INTEGER NOT NULL,
  dest_file_id INTEGER,
  created_by INTEGER NOT NULL
)`)
	require.NoError(t, err)
	return client
}

func seedSymbolicStub(t *testing.T, client *ent.Client) (parent *ent.File, stub *ent.File, transferID, srcTeamID, dstTeamID int) {
	t.Helper()
	ctx := context.Background()
	perms := boolset.BooleanSet{}
	group, err := client.Group.Create().SetName("g").SetPermissions(&perms).Save(ctx)
	require.NoError(t, err)
	user, err := client.User.Create().SetUsername("u").SetNick("n").SetGroup(group).Save(ctx)
	require.NoError(t, err)
	parent, err = client.File.Create().
		SetType(int(types.FileTypeFolder)).
		SetName("Incoming").
		SetOwner(user).
		Save(ctx)
	require.NoError(t, err)
	stub, err = client.File.Create().
		SetType(int(types.FileTypeFile)).
		SetName("doc.txt").
		SetOwner(user).
		SetIsSymbolic(true).
		SetParent(parent).
		Save(ctx)
	require.NoError(t, err)
	srcTeam, err := client.Team.Create().SetName("src").SetRoot(parent).Save(ctx)
	require.NoError(t, err)
	dstRoot, err := client.File.Create().
		SetType(int(types.FileTypeFolder)).
		SetName("dst-root").
		SetOwner(user).
		Save(ctx)
	require.NoError(t, err)
	dstTeam, err := client.Team.Create().SetName("dst").SetRoot(dstRoot).Save(ctx)
	require.NoError(t, err)
	res, err := client.ExecContext(ctx, `
INSERT INTO team_transfers
  (created_at, updated_at, allow_write, source_file_id, source_team_id, dest_team_id, dest_file_id, created_by)
VALUES (CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 0, ?, ?, ?, ?, ?)`,
		parent.ID, srcTeam.ID, dstTeam.ID, stub.ID, user.ID)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return parent, stub, int(id), srcTeam.ID, dstTeam.ID
}

func TestDeleteTeamTransferStubLoadsEntities(t *testing.T) {
	ctx := context.Background()
	client := openTransferTestDB(t)
	_, stub, transferID, _, _ := seedSymbolicStub(t, client)
	hasher, err := hashid.New("test-salt")
	require.NoError(t, err)
	fc := inventory.NewFileClient(client, conf.SQLite3DB, hasher)
	tc := inventory.NewTeamTransferClient(client, conf.SQLite3DB)

	// Without LoadFileEntity, Delete hits NotLoadedError and the soft-delete must roll back.
	fcTx, tx, txCtx, err := inventory.WithTx(ctx, fc)
	require.NoError(t, err)
	tcTx, _ := inventory.InheritTx(txCtx, tc)
	require.NoError(t, tcTx.SoftDelete(txCtx, transferID))
	unloaded, err := fcTx.GetByID(txCtx, stub.ID)
	require.NoError(t, err)
	_, _, err = fcTx.Delete(txCtx, []*ent.File{unloaded}, nil)
	require.Error(t, err)
	require.NoError(t, inventory.Rollback(tx))

	deleted, sourceID, destID := scanTransferRow(t, client, transferID)
	require.False(t, deleted.Valid, "soft-delete must roll back when stub delete fails")
	require.True(t, destID.Valid)

	// Same sequence as DeleteTeamTransfer: load entities, then delete in the tx.
	fcTx, tx, txCtx, err = inventory.WithTx(ctx, fc)
	require.NoError(t, err)
	txCtx = context.WithValue(txCtx, inventory.LoadFileEntity{}, true)
	tcTx, _ = inventory.InheritTx(txCtx, tc)
	require.NoError(t, tcTx.SoftDelete(txCtx, transferID))
	loaded, err := fcTx.GetByID(txCtx, stub.ID)
	require.NoError(t, err)
	_, err = loaded.Edges.EntitiesOrErr()
	require.NoError(t, err)
	_, _, err = fcTx.Delete(txCtx, []*ent.File{loaded}, nil)
	require.NoError(t, err)
	require.NoError(t, inventory.Commit(tx))

	deleted, sourceID, destID = scanTransferRow(t, client, transferID)
	require.True(t, deleted.Valid)
	require.False(t, sourceID.Valid)
	require.False(t, destID.Valid)
	_, err = client.File.Get(ctx, stub.ID)
	require.True(t, ent.IsNotFound(err))
}

func TestCreateTransferStubRetriesName(t *testing.T) {
	ctx := context.Background()
	client := openTransferTestDB(t)
	parent, _, _, srcTeamID, dstTeamID := seedSymbolicStub(t, client)
	hasher, err := hashid.New("test-salt")
	require.NoError(t, err)
	fc := inventory.NewFileClient(client, conf.SQLite3DB, hasher)
	tc := inventory.NewTeamTransferClient(client, conf.SQLite3DB)

	fcTx, tx, txCtx, err := inventory.WithTx(ctx, fc)
	require.NoError(t, err)
	tcTx, _ := inventory.InheritTx(txCtx, tc)
	policy, err := client.StoragePolicy.Create().SetName("local").SetType("local").Save(ctx)
	require.NoError(t, err)
	parent.StoragePolicyFiles = policy.ID
	_, name, tr, err := createTransferStubInTx(txCtx, fcTx, tcTx, hasher, parent, "doc.txt", false, &inventory.CreateTeamTransferParams{
		SourceFileID: parent.ID,
		SourceTeamID: srcTeamID,
		DestTeamID:   dstTeamID,
		CreatedBy:    parent.OwnerID,
		AllowWrite:   false,
	}, map[string]string{"sys:transfer_from_team_name": "A"})
	require.NoError(t, err)
	require.Equal(t, "doc (1).txt", name)
	require.NotZero(t, tr.ID)
	require.NoError(t, inventory.Commit(tx))

	rows, err := client.QueryContext(ctx, `
SELECT value FROM metadata WHERE file_id = ? AND name = 'sys:team_transfer_redirect'`, tr.DestFileID)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	var redirect string
	require.NoError(t, rows.Scan(&redirect))
	require.NotContains(t, redirect, "pending@")
	require.Contains(t, redirect, "@teamtransfer")
}

// hideChildOnce makes the pre-check miss one committed name so CreateFolder/CreateFile
// run against a row that already exists.
type hideChildOnce struct {
	inventory.FileClient
	name string
	used bool
}

func (h *hideChildOnce) GetChildFile(ctx context.Context, root *ent.File, ownerID int, child string, eagerLoading bool) (*ent.File, error) {
	if !h.used && child == h.name {
		h.used = true
		return nil, &ent.NotFoundError{}
	}
	return h.FileClient.GetChildFile(ctx, root, ownerID, child, eagerLoading)
}

func assertNoTransferRedirect(t *testing.T, client *ent.Client, fileID int) {
	t.Helper()
	rows, err := client.QueryContext(context.Background(), `
SELECT COUNT(*) FROM metadata WHERE file_id = ? AND name = 'sys:team_transfer_redirect'`, fileID)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	var n int
	require.NoError(t, rows.Scan(&n))
	require.Zero(t, n)
}

func assertTransferRedirect(t *testing.T, client *ent.Client, fileID int) {
	t.Helper()
	rows, err := client.QueryContext(context.Background(), `
SELECT value FROM metadata WHERE file_id = ? AND name = 'sys:team_transfer_redirect'`, fileID)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	var redirect string
	require.NoError(t, rows.Scan(&redirect))
	require.NotContains(t, redirect, "pending@")
	require.Contains(t, redirect, "@teamtransfer")
}

func TestCreateFolderStubSkipsExistingChild(t *testing.T) {
	ctx := context.Background()
	client := openTransferTestDB(t)
	parent, _, _, srcTeamID, dstTeamID := seedSymbolicStub(t, client)
	existing, err := client.File.Create().
		SetType(int(types.FileTypeFolder)).
		SetName("Reports").
		SetOwnerID(parent.OwnerID).
		SetIsSymbolic(false).
		SetParent(parent).
		Save(ctx)
	require.NoError(t, err)

	hasher, err := hashid.New("test-salt")
	require.NoError(t, err)
	fc := inventory.NewFileClient(client, conf.SQLite3DB, hasher)
	tc := inventory.NewTeamTransferClient(client, conf.SQLite3DB)
	fcTx, tx, txCtx, err := inventory.WithTx(ctx, fc)
	require.NoError(t, err)
	tcTx, _ := inventory.InheritTx(txCtx, tc)

	stub, name, _, err := createTransferStubInTx(txCtx, fcTx, tcTx, hasher, parent, "Reports", true, &inventory.CreateTeamTransferParams{
		SourceFileID: parent.ID,
		SourceTeamID: srcTeamID,
		DestTeamID:   dstTeamID,
		CreatedBy:    parent.OwnerID,
	}, map[string]string{"sys:transfer_from_team_name": "A"})
	require.NoError(t, err)
	require.Equal(t, "Reports (1)", name)
	require.NotEqual(t, existing.ID, stub.ID)
	require.NoError(t, inventory.Commit(tx))

	got, err := client.File.Get(ctx, existing.ID)
	require.NoError(t, err)
	require.False(t, got.IsSymbolic)
	assertNoTransferRedirect(t, client, existing.ID)

	created, err := client.File.Get(ctx, stub.ID)
	require.NoError(t, err)
	require.True(t, created.IsSymbolic)
	require.Equal(t, "Reports (1)", created.Name)
	assertTransferRedirect(t, client, stub.ID)
}

func TestCreateFolderStubRollsBackRacedExisting(t *testing.T) {
	ctx := context.Background()
	client := openTransferTestDB(t)
	parent, _, _, srcTeamID, dstTeamID := seedSymbolicStub(t, client)
	existing, err := client.File.Create().
		SetType(int(types.FileTypeFolder)).
		SetName("Reports").
		SetOwnerID(parent.OwnerID).
		SetIsSymbolic(false).
		SetParent(parent).
		Save(ctx)
	require.NoError(t, err)

	hasher, err := hashid.New("test-salt")
	require.NoError(t, err)
	fc := inventory.NewFileClient(client, conf.SQLite3DB, hasher)
	tc := inventory.NewTeamTransferClient(client, conf.SQLite3DB)
	fcTx, tx, txCtx, err := inventory.WithTx(ctx, fc)
	require.NoError(t, err)
	tcTx, _ := inventory.InheritTx(txCtx, tc)
	hidden := &hideChildOnce{FileClient: fcTx, name: "Reports"}

	stub, name, _, err := createTransferStubInTx(txCtx, hidden, tcTx, hasher, parent, "Reports", true, &inventory.CreateTeamTransferParams{
		SourceFileID: parent.ID,
		SourceTeamID: srcTeamID,
		DestTeamID:   dstTeamID,
		CreatedBy:    parent.OwnerID,
	}, map[string]string{"sys:transfer_from_team_name": "A"})
	require.NoError(t, err)
	require.True(t, hidden.used)
	require.Equal(t, "Reports (1)", name)
	require.NotEqual(t, existing.ID, stub.ID)
	require.NoError(t, inventory.Commit(tx))

	got, err := client.File.Get(ctx, existing.ID)
	require.NoError(t, err)
	require.False(t, got.IsSymbolic)
	assertNoTransferRedirect(t, client, existing.ID)

	created, err := client.File.Get(ctx, stub.ID)
	require.NoError(t, err)
	require.True(t, created.IsSymbolic)
	assertTransferRedirect(t, client, stub.ID)
}

func TestCreateFileStubRollsBackUniqueInsideSavepoint(t *testing.T) {
	ctx := context.Background()
	client := openTransferTestDB(t)
	parent, existing, _, srcTeamID, dstTeamID := seedSymbolicStub(t, client)
	policy, err := client.StoragePolicy.Create().SetName("local").SetType("local").Save(ctx)
	require.NoError(t, err)
	parent.StoragePolicyFiles = policy.ID

	hasher, err := hashid.New("test-salt")
	require.NoError(t, err)
	fc := inventory.NewFileClient(client, conf.SQLite3DB, hasher)
	tc := inventory.NewTeamTransferClient(client, conf.SQLite3DB)
	fcTx, tx, txCtx, err := inventory.WithTx(ctx, fc)
	require.NoError(t, err)
	tcTx, _ := inventory.InheritTx(txCtx, tc)
	hidden := &hideChildOnce{FileClient: fcTx, name: "doc.txt"}

	stub, name, _, err := createTransferStubInTx(txCtx, hidden, tcTx, hasher, parent, "doc.txt", false, &inventory.CreateTeamTransferParams{
		SourceFileID: parent.ID,
		SourceTeamID: srcTeamID,
		DestTeamID:   dstTeamID,
		CreatedBy:    parent.OwnerID,
	}, map[string]string{"sys:transfer_from_team_name": "A"})
	require.NoError(t, err)
	require.True(t, hidden.used)
	require.Equal(t, "doc (1).txt", name)
	require.NotEqual(t, existing.ID, stub.ID)
	require.NoError(t, inventory.Commit(tx))

	got, err := client.File.Get(ctx, existing.ID)
	require.NoError(t, err)
	require.Equal(t, "doc.txt", got.Name)
	assertNoTransferRedirect(t, client, existing.ID)
	assertTransferRedirect(t, client, stub.ID)
}

func scanTransferRow(t *testing.T, client *ent.Client, id int) (sql.NullString, sql.NullInt64, sql.NullInt64) {
	t.Helper()
	rows, err := client.QueryContext(context.Background(), `SELECT deleted_at, source_file_id, dest_file_id FROM team_transfers WHERE id = ?`, id)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	var deleted sql.NullString
	var sourceID, destID sql.NullInt64
	require.NoError(t, rows.Scan(&deleted, &sourceID, &destID))
	return deleted, sourceID, destID
}

package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// TeamTransfer is a live link of a source team file into a destination team.
type TeamTransfer struct {
	ent.Schema
}

func (TeamTransfer) Fields() []ent.Field {
	return []ent.Field{
		// Nullable so revoke / source delete can clear FKs before file hard-delete.
		field.Int("source_file_id").
			Optional().
			Nillable(),
		field.Int("source_team_id"),
		field.Int("dest_team_id"),
		field.Int("dest_file_id").
			Optional().
			Nillable(),
		field.Int("created_by"),
		field.Bool("allow_write").
			Default(false),
	}
}

func (TeamTransfer) Mixin() []ent.Mixin {
	return []ent.Mixin{
		CommonMixin{},
	}
}

func (TeamTransfer) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("source_file", File.Type).
			Ref("outgoing_team_transfers").
			Field("source_file_id"),
		edge.From("source_team", Team.Type).
			Ref("outgoing_transfers").
			Field("source_team_id").
			Required(),
		edge.From("dest_team", Team.Type).
			Ref("incoming_transfers").
			Field("dest_team_id").
			Required(),
		// Each destination stub maps to at most one active transfer.
		edge.From("dest_file", File.Type).
			Ref("incoming_team_transfer").
			Field("dest_file_id").
			Unique(),
		edge.From("creator", User.Type).
			Ref("created_team_transfers").
			Field("created_by").
			Required(),
	}
}

func (TeamTransfer) Indexes() []ent.Index {
	return []ent.Index{
		// Unique dest_file_id: multiple NULLs after revoke are allowed by SQL.
		index.Fields("dest_file_id").Unique(),
		index.Fields("dest_team_id"),
		index.Fields("source_file_id"),
		index.Fields("source_team_id"),
	}
}

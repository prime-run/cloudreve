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
		field.Int("source_file_id"),
		field.Int("source_team_id"),
		field.Int("dest_team_id"),
		field.Int("dest_file_id"),
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
		// Many transfers can reference the same source file / team / creator.
		edge.From("source_file", File.Type).
			Ref("outgoing_team_transfers").
			Field("source_file_id").
			Required(),
		edge.From("source_team", Team.Type).
			Ref("outgoing_transfers").
			Field("source_team_id").
			Required(),
		edge.From("dest_team", Team.Type).
			Ref("incoming_transfers").
			Field("dest_team_id").
			Required(),
		// Each destination stub maps to at most one transfer.
		edge.From("dest_file", File.Type).
			Ref("incoming_team_transfer").
			Field("dest_file_id").
			Unique().
			Required(),
		edge.From("creator", User.Type).
			Ref("created_team_transfers").
			Field("created_by").
			Required(),
	}
}

func (TeamTransfer) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("dest_file_id").Unique(),
		index.Fields("dest_team_id"),
		index.Fields("source_file_id"),
		index.Fields("source_team_id"),
	}
}

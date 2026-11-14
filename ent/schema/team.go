package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// Team holds the schema definition for shared team workspaces.
type Team struct {
	ent.Schema
}

func (Team) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").
			MaxLen(255),
		field.Int("root_file_id"),
		// MaxStorage is the shared storage quota for the whole team drive (bytes).
		// Optional: 0 / unset means no limit, matching Group.MaxStorage semantics.
		field.Int64("max_storage").
			Optional(),
		// Storage is the currently used shared storage under the team root (bytes).
		field.Int64("storage").
			Default(0),
	}
}

func (Team) Mixin() []ent.Mixin {
	return []ent.Mixin{
		CommonMixin{},
	}
}

func (Team) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("root", File.Type).
			Field("root_file_id").
			Unique().
			Required(),
		edge.To("members", TeamMember.Type),
		edge.To("outgoing_transfers", TeamTransfer.Type),
		edge.To("incoming_transfers", TeamTransfer.Type),
	}
}

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
	}
}

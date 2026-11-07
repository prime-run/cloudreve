package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// TeamMember holds membership of a user in a Team.
type TeamMember struct {
	ent.Schema
}

func (TeamMember) Fields() []ent.Field {
	return []ent.Field{
		field.Int("team_id"),
		field.Int("user_id"),
		field.Enum("role").
			Values("owner", "admin", "member").
			Default("member"),
	}
}

func (TeamMember) Mixin() []ent.Mixin {
	return []ent.Mixin{
		CommonMixin{},
	}
}

func (TeamMember) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("team", Team.Type).
			Ref("members").
			Field("team_id").
			Unique().
			Required(),
		edge.From("user", User.Type).
			Ref("team_memberships").
			Field("user_id").
			Unique().
			Required(),
	}
}

func (TeamMember) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("team_id", "user_id").
			Unique(),
	}
}

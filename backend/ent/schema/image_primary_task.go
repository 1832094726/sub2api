package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type ImagePrimaryTask struct {
	ent.Schema
}

func (ImagePrimaryTask) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "image_primary_tasks"}}
}

func (ImagePrimaryTask) Fields() []ent.Field {
	return []ent.Field{
		field.String("public_id").MaxLen(64).Immutable().Unique(),
		field.Int64("user_id"),
		field.Int64("api_key_id"),
		field.Int64("usage_log_id").Optional().Nillable(),
		field.String("protocol").MaxLen(32),
		field.String("model").MaxLen(128),
		field.String("request_hash").MaxLen(64),
		field.String("upstream_task_id").MaxLen(128).Optional().Nillable(),
		field.String("status").MaxLen(16).Default("queued"),
		field.String("fallback_reason").MaxLen(128).Optional().Nillable(),
		field.String("result_locator").SchemaType(map[string]string{dialect.Postgres: "text"}).Optional().Nillable(),
		field.Int("image_count").Default(0),
		field.String("image_size").MaxLen(32).Optional().Nillable(),
		field.Int64("primary_duration_ms").Default(0),
		field.Int64("fallback_duration_ms").Default(0),
		field.String("settlement_state").MaxLen(16).Default("pending"),
		field.Time("created_at").Immutable().Default(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("expires_at").SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (ImagePrimaryTask) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("api_key_id", "public_id"),
		index.Fields("status", "updated_at"),
		index.Fields("expires_at"),
	}
}

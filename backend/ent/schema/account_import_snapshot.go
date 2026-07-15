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

type AccountImportSnapshot struct {
	ent.Schema
}

func (AccountImportSnapshot) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "account_import_snapshots"}}
}

func (AccountImportSnapshot) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("account_id").Unique(),
		field.String("batch_id").MaxLen(64),
		field.String("encrypted_json").SchemaType(map[string]string{dialect.Postgres: "text"}),
		field.Time("imported_at").Default(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (AccountImportSnapshot) Indexes() []ent.Index {
	return []ent.Index{index.Fields("imported_at")}
}

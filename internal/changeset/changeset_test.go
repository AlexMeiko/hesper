package changeset_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"hesper/internal/changeset"
	"hesper/internal/driver"
	"hesper/internal/driver/fake"
	"hesper/internal/dto"
)

func sampleMySQLWriteSchema() driver.WriteSchema {
	return driver.WriteSchema{
		Dialect:       dto.EngineMySQL,
		Database:      "demo",
		Table:         "users",
		StorageEngine: "InnoDB",
		Editability: dto.TableEditability{
			CanWrite:           true,
			RowIdentity:        dto.RowIdentityPrimaryKey,
			KeyColumns:         []string{"id"},
			BatchTransactional: true,
			StorageEngine:      "InnoDB",
		},
		Columns: []driver.WriteColumn{
			{
				Detail: dto.ColumnDetail{
					Name:          "id",
					DataType:      "bigint",
					ColumnType:    "bigint",
					Nullable:      false,
					IsPrimaryKey:  true,
					AutoIncrement: true,
					Insertable:    true,
					Updatable:     true,
					OrdinalPos:    1,
				},
				NativeType:      "bigint",
				Deterministic:   true,
				CompareStrategy: driver.CompareExact,
				KeyComparable:   true,
			},
			{
				Detail: dto.ColumnDetail{
					Name:       "name",
					DataType:   "varchar",
					ColumnType: "varchar(100)",
					Collation:  "utf8mb4_0900_ai_ci",
					Nullable:   true,
					Insertable: true,
					Updatable:  true,
					OrdinalPos: 2,
				},
				NativeType:      "varchar",
				Collation:       "utf8mb4_0900_ai_ci",
				Deterministic:   true,
				CompareStrategy: driver.CompareMySQLBinary,
				KeyComparable:   true,
			},
			{
				Detail: dto.ColumnDetail{
					Name:         "amount",
					DataType:     "decimal",
					ColumnType:   "decimal(38,10)",
					Nullable:     true,
					HasDefault:   true,
					DefaultValue: "0.0000000000",
					Insertable:   true,
					Updatable:    true,
					OrdinalPos:   3,
				},
				NativeType:      "decimal",
				Deterministic:   true,
				CompareStrategy: driver.CompareExact,
				KeyComparable:   true,
			},
			{
				Detail: dto.ColumnDetail{
					Name:           "full_label",
					DataType:       "varchar",
					ColumnType:     "varchar(120)",
					Nullable:       true,
					GeneratedKind:  "virtual",
					Insertable:     false,
					Updatable:      false,
					ReadOnlyReason: "生成列不可直接写入",
					OrdinalPos:     4,
				},
				NativeType:      "varchar",
				Deterministic:   true,
				CompareStrategy: driver.CompareMySQLBinary,
				KeyComparable:   false,
			},
		},
	}
}

func sampleSourceRef() changeset.SourceRef {
	return changeset.SourceRef{
		ConnectionID:   "conn-1",
		ConfigVersion:  1,
		LifecycleEpoch: 0,
		Target: changeset.ResolvedTarget{
			Engine:   dto.EngineMySQL,
			Host:     "127.0.0.1",
			Port:     3306,
			Database: "demo",
			Table:    "users",
		},
	}
}

func assertAppErrorCode(t *testing.T, err error, want dto.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", want)
	}
	var ae *dto.AppError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *dto.AppError(%s), got %T: %v", want, err, err)
	}
	if ae.Code != want {
		t.Fatalf("expected error code %s, got %s (msg=%q detail=%q)", want, ae.Code, ae.Message, ae.Detail)
	}
}

func TestSchemaFingerprintDetectsFullColumnTypeAndWriteSemantics(t *testing.T) {
	base := sampleMySQLWriteSchema()
	fpBase := changeset.ComputeSchemaFingerprint(base)

	mutations := []struct {
		name   string
		mutate func(ws *driver.WriteSchema)
	}{
		{
			name: "MySQL DECIMAL(38,10) -> DECIMAL(38,2) with identical NativeType",
			mutate: func(ws *driver.WriteSchema) {
				ws.Columns[2].Detail.ColumnType = "decimal(38,2)"
			},
		},
		{
			name: "VARCHAR(100) -> VARCHAR(10)",
			mutate: func(ws *driver.WriteSchema) {
				ws.Columns[1].Detail.ColumnType = "varchar(10)"
			},
		},
		{
			name: "HasDefault changed",
			mutate: func(ws *driver.WriteSchema) {
				ws.Columns[2].Detail.HasDefault = false
			},
		},
		{
			name: "DefaultValue changed",
			mutate: func(ws *driver.WriteSchema) {
				ws.Columns[2].Detail.DefaultValue = "1.0000000000"
			},
		},
		{
			name: "AutoIncrement changed",
			mutate: func(ws *driver.WriteSchema) {
				ws.Columns[0].Detail.AutoIncrement = false
			},
		},
		{
			name: "Collation changed",
			mutate: func(ws *driver.WriteSchema) {
				ws.Columns[1].Collation = "utf8mb4_bin"
			},
		},
		{
			name: "StorageEngine changed InnoDB -> MyISAM",
			mutate: func(ws *driver.WriteSchema) {
				ws.StorageEngine = "MyISAM"
			},
		},
		{
			name: "SQLite Strict flag changed",
			mutate: func(ws *driver.WriteSchema) {
				ws.Strict = true
			},
		},
	}

	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			cp := sampleMySQLWriteSchema()
			tc.mutate(&cp)
			fpMut := changeset.ComputeSchemaFingerprint(cp)
			if fpMut == fpBase {
				t.Fatalf("%s: expected SchemaFingerprint to change, stayed %s", tc.name, fpBase)
			}
		})
	}
}

func TestBuildChangePlanNoopNullPKUpdateAndOrder(t *testing.T) {
	ws := sampleMySQLWriteSchema()
	src := sampleSourceRef()

	t.Run("whole-row no-op emits zero UPDATE statements", func(t *testing.T) {
		plan, previews, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "conn-1",
			Database:     "demo",
			Table:        "users",
			Updates: []dto.GridRowUpdate{
				{
					RowKey: "row-1",
					KeyValues: map[string]dto.OriginalCellValue{
						"id": {Kind: dto.KindInt, Text: "1"},
					},
					OriginalValues: map[string]dto.OriginalCellValue{
						"name":   {Kind: dto.KindString, Text: "Alice"},
						"amount": {Kind: dto.KindNull},
					},
					NewValues: map[string]dto.NewCellValue{
						"name":   {Mode: dto.CellInputLiteral, Text: "Alice"},
						"amount": {Mode: dto.CellInputNull},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(plan.Steps) != 0 || len(previews) != 0 || plan.UpdateCount != 0 {
			t.Fatalf("expected 0 steps for restored no-op row, got %d steps: %+v", len(plan.Steps), previews)
		}
	})

	t.Run("NULL original generates IS NULL and distinguishes empty string from NULL", func(t *testing.T) {
		plan, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "conn-1",
			Database:     "demo",
			Table:        "users",
			Updates: []dto.GridRowUpdate{
				{
					RowKey: "row-null",
					KeyValues: map[string]dto.OriginalCellValue{
						"id": {Kind: dto.KindInt, Text: "10"},
					},
					OriginalValues: map[string]dto.OriginalCellValue{
						"name": {Kind: dto.KindNull},
					},
					NewValues: map[string]dto.NewCellValue{
						"name": {Mode: dto.CellInputLiteral, Text: ""},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(plan.Steps) != 1 {
			t.Fatalf("expected 1 update step (NULL -> empty string is NOT a no-op), got %d", len(plan.Steps))
		}
		sqlStr := plan.Steps[0].Stmt.SQL
		if !strings.Contains(sqlStr, "`name` IS NULL") {
			t.Fatalf("expected `name` IS NULL in WHERE, got: %s", sqlStr)
		}
		if strings.Contains(sqlStr, "= NULL") {
			t.Fatalf("forbidden '= NULL' in SQL: %s", sqlStr)
		}
		// SET name = ? (arg ""), WHERE id = ? (arg "10") AND name IS NULL (0 args) -> 2 total args.
		if len(plan.Steps[0].Stmt.Args) != 2 {
			t.Fatalf("expected 2 bound args (SET + PK), got %d: %#v", len(plan.Steps[0].Stmt.Args), plan.Steps[0].Stmt.Args)
		}
	})

	t.Run("updating PK column uses original PK in WHERE without duplicating predicate", func(t *testing.T) {
		plan, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "conn-1",
			Database:     "demo",
			Table:        "users",
			Updates: []dto.GridRowUpdate{
				{
					RowKey: "row-pk",
					KeyValues: map[string]dto.OriginalCellValue{
						"id": {Kind: dto.KindInt, Text: "7"},
					},
					OriginalValues: map[string]dto.OriginalCellValue{
						"name": {Kind: dto.KindString, Text: "Old"},
					},
					NewValues: map[string]dto.NewCellValue{
						"id":   {Mode: dto.CellInputLiteral, Text: "70"},
						"name": {Mode: dto.CellInputLiteral, Text: "New"},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(plan.Steps) != 1 {
			t.Fatalf("expected 1 step, got %d", len(plan.Steps))
		}
		st := plan.Steps[0]
		if !strings.Contains(st.Stmt.SQL, "SET `id` = ?, `name` = ? WHERE `id` = ? AND CAST(CONVERT(`name` USING utf8mb4) AS BINARY) = CAST(CONVERT(? USING utf8mb4) AS BINARY)") {
			t.Fatalf("unexpected PK-update SQL: %s", st.Stmt.SQL)
		}
		// Args: new id "70", new name "New", orig id "7", orig name "Old".
		wantArgs := []string{"70", "New", "7", "Old"}
		if len(st.Stmt.Args) != len(wantArgs) {
			t.Fatalf("expected %d args, got %#v", len(wantArgs), st.Stmt.Args)
		}
		for i, w := range wantArgs {
			if st.Stmt.Args[i] != w {
				t.Fatalf("arg[%d]: want %q, got %#v", i, w, st.Stmt.Args[i])
			}
		}
	})

	t.Run("fixed order Deletes -> Updates -> Inserts and generated col omission", func(t *testing.T) {
		plan, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "conn-1",
			Database:     "demo",
			Table:        "users",
			Inserts: []dto.GridRowInsert{
				{
					RowKey: "ins-1",
					Values: map[string]dto.NewCellValue{
						"name":       {Mode: dto.CellInputLiteral, Text: "Bob"},
						"full_label": {Mode: dto.CellInputDefault}, // default allowed to omit generated col
					},
				},
			},
			Updates: []dto.GridRowUpdate{
				{
					RowKey:    "upd-1",
					KeyValues: map[string]dto.OriginalCellValue{"id": {Kind: dto.KindInt, Text: "2"}},
					OriginalValues: map[string]dto.OriginalCellValue{
						"name": {Kind: dto.KindString, Text: "A"},
					},
					NewValues: map[string]dto.NewCellValue{
						"name": {Mode: dto.CellInputLiteral, Text: "B"},
					},
				},
			},
			Deletes: []dto.GridRowDelete{
				{
					RowKey:    "del-1",
					KeyValues: map[string]dto.OriginalCellValue{"id": {Kind: dto.KindInt, Text: "1"}},
				},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(plan.Steps) != 3 {
			t.Fatalf("expected 3 steps, got %d", len(plan.Steps))
		}
		if plan.Steps[0].Op != changeset.OpDelete || plan.Steps[1].Op != changeset.OpUpdate || plan.Steps[2].Op != changeset.OpInsert {
			t.Fatalf("expected Delete -> Update -> Insert order, got %s -> %s -> %s",
				plan.Steps[0].Op, plan.Steps[1].Op, plan.Steps[2].Op)
		}
	})
}

func TestBuildChangePlanGuardsAndKeyCompleteness(t *testing.T) {
	src := sampleSourceRef()

	t.Run("truncated stable key rejects UPDATE and DELETE", func(t *testing.T) {
		ws := sampleMySQLWriteSchema()
		_, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "conn-1",
			Database:     "demo",
			Table:        "users",
			Deletes: []dto.GridRowDelete{
				{
					RowKey: "del-trunc",
					KeyValues: map[string]dto.OriginalCellValue{
						"id": {Kind: dto.KindInt, Text: "1", Truncated: true},
					},
				},
			},
		})
		assertAppErrorCode(t, err, dto.ErrRowNotIdentifiable)
	})

	t.Run("binary or NULL stable key rejects UPDATE and DELETE", func(t *testing.T) {
		ws := sampleMySQLWriteSchema()
		_, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "conn-1",
			Database:     "demo",
			Table:        "users",
			Deletes: []dto.GridRowDelete{
				{
					RowKey:    "del-bin",
					KeyValues: map[string]dto.OriginalCellValue{"id": {Kind: dto.KindBinary, Text: "0x01"}},
				},
			},
		})
		assertAppErrorCode(t, err, dto.ErrRowNotIdentifiable)

		_, _, err = changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "conn-1",
			Database:     "demo",
			Table:        "users",
			Deletes: []dto.GridRowDelete{
				{
					RowKey:    "del-null",
					KeyValues: map[string]dto.OriginalCellValue{"id": {Kind: dto.KindNull}},
				},
			},
		})
		assertAppErrorCode(t, err, dto.ErrRowNotIdentifiable)
	})

	t.Run("missing stable key column rejects UPDATE", func(t *testing.T) {
		ws := sampleMySQLWriteSchema()
		_, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "conn-1",
			Database:     "demo",
			Table:        "users",
			Updates: []dto.GridRowUpdate{
				{
					RowKey:         "upd-missing-key",
					KeyValues:      map[string]dto.OriginalCellValue{},
					OriginalValues: map[string]dto.OriginalCellValue{"name": {Kind: dto.KindString, Text: "A"}},
					NewValues:      map[string]dto.NewCellValue{"name": {Mode: dto.CellInputLiteral, Text: "B"}},
				},
			},
		})
		assertAppErrorCode(t, err, dto.ErrRowNotIdentifiable)
	})

	t.Run("table with RowIdentityNone allows INSERT and rejects UPDATE/DELETE", func(t *testing.T) {
		ws := sampleMySQLWriteSchema()
		ws.Editability.RowIdentity = dto.RowIdentityNone
		ws.Editability.KeyColumns = nil

		plan, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "conn-1",
			Database:     "demo",
			Table:        "users",
			Inserts: []dto.GridRowInsert{
				{RowKey: "ins-only", Values: map[string]dto.NewCellValue{"name": {Mode: dto.CellInputLiteral, Text: "OK"}}},
			},
		})
		if err != nil || len(plan.Steps) != 1 {
			t.Fatalf("expected INSERT to succeed on RowIdentityNone table, got err=%v", err)
		}

		_, _, err = changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "conn-1",
			Database:     "demo",
			Table:        "users",
			Deletes:      []dto.GridRowDelete{{RowKey: "del-1", KeyValues: map[string]dto.OriginalCellValue{"id": {Kind: dto.KindInt, Text: "1"}}}},
		})
		assertAppErrorCode(t, err, dto.ErrRowNotIdentifiable)
	})

	t.Run("generated column explicit write rejected on INSERT and UPDATE", func(t *testing.T) {
		ws := sampleMySQLWriteSchema()
		_, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "conn-1",
			Database:     "demo",
			Table:        "users",
			Inserts: []dto.GridRowInsert{
				{RowKey: "ins-gen", Values: map[string]dto.NewCellValue{"full_label": {Mode: dto.CellInputLiteral, Text: "x"}}},
			},
		})
		assertAppErrorCode(t, err, dto.ErrInvalidArgument)

		_, _, err = changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "conn-1",
			Database:     "demo",
			Table:        "users",
			Updates: []dto.GridRowUpdate{
				{
					RowKey:         "upd-gen",
					KeyValues:      map[string]dto.OriginalCellValue{"id": {Kind: dto.KindInt, Text: "1"}},
					OriginalValues: map[string]dto.OriginalCellValue{"full_label": {Kind: dto.KindString, Text: "a"}},
					NewValues:      map[string]dto.NewCellValue{"full_label": {Mode: dto.CellInputLiteral, Text: "b"}},
				},
			},
		})
		assertAppErrorCode(t, err, dto.ErrInvalidArgument)
	})

	t.Run("UPDATE rejects CellInputDefault and invalid input mode", func(t *testing.T) {
		ws := sampleMySQLWriteSchema()
		_, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "conn-1",
			Database:     "demo",
			Table:        "users",
			Updates: []dto.GridRowUpdate{
				{
					RowKey:         "upd-def",
					KeyValues:      map[string]dto.OriginalCellValue{"id": {Kind: dto.KindInt, Text: "1"}},
					OriginalValues: map[string]dto.OriginalCellValue{"name": {Kind: dto.KindString, Text: "a"}},
					NewValues:      map[string]dto.NewCellValue{"name": {Mode: dto.CellInputDefault}},
				},
			},
		})
		assertAppErrorCode(t, err, dto.ErrInvalidArgument)
	})

	t.Run("non-transactional engine rejected with ErrBatchNonTx", func(t *testing.T) {
		ws := sampleMySQLWriteSchema()
		ws.StorageEngine = "MyISAM"
		ws.Editability.BatchTransactional = false
		_, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "conn-1",
			Database:     "demo",
			Table:        "users",
			Inserts:      []dto.GridRowInsert{{RowKey: "ins", Values: map[string]dto.NewCellValue{"name": {Mode: dto.CellInputLiteral, Text: "x"}}}},
		})
		assertAppErrorCode(t, err, dto.ErrBatchNonTx)
	})
}

func TestSQLiteNewValueVsOriginalValueSeparation(t *testing.T) {
	ws := driver.WriteSchema{
		Dialect:       dto.EngineSQLite,
		Database:      "main",
		Table:         "items",
		StorageEngine: "SQLite",
		Editability: dto.TableEditability{
			CanWrite:           true,
			RowIdentity:        dto.RowIdentityPrimaryKey,
			KeyColumns:         []string{"id"},
			BatchTransactional: true,
			StorageEngine:      "SQLite",
		},
		Columns: []driver.WriteColumn{
			{
				Detail:          dto.ColumnDetail{Name: "id", DataType: "INTEGER", ColumnType: "INTEGER", IsPrimaryKey: true, Insertable: true, Updatable: true, OrdinalPos: 1},
				NativeType:      "INTEGER",
				Deterministic:   true,
				SQLiteAffinity:  "integer",
				CompareStrategy: driver.CompareSQLiteBinaryTypeof,
				KeyComparable:   true,
			},
			{
				Detail:          dto.ColumnDetail{Name: "ratio", DataType: "REAL", ColumnType: "REAL", Nullable: true, Insertable: true, Updatable: true, OrdinalPos: 2},
				NativeType:      "REAL",
				Deterministic:   true,
				SQLiteAffinity:  "real",
				CompareStrategy: driver.CompareSQLiteBinaryTypeof,
				KeyComparable:   false,
			},
			{
				Detail:          dto.ColumnDetail{Name: "amt", DataType: "DECIMAL(38,10)", ColumnType: "DECIMAL(38,10)", Nullable: true, Insertable: true, Updatable: true, OrdinalPos: 3},
				NativeType:      "DECIMAL(38,10)",
				Deterministic:   true,
				SQLiteAffinity:  "numeric",
				CompareStrategy: driver.CompareSQLiteBinaryTypeof,
				KeyComparable:   true,
			},
			{
				Detail:          dto.ColumnDetail{Name: "created_at", DataType: "DATETIME", ColumnType: "DATETIME", Nullable: true, Insertable: true, Updatable: true, OrdinalPos: 4},
				NativeType:      "DATETIME",
				Deterministic:   true,
				SQLiteAffinity:  "numeric",
				CompareStrategy: driver.CompareSQLiteBinaryTypeof,
				KeyComparable:   true,
			},
			{
				Detail:          dto.ColumnDetail{Name: "note", DataType: "TEXT", ColumnType: "TEXT", Nullable: true, Insertable: true, Updatable: true, OrdinalPos: 5},
				NativeType:      "TEXT",
				Deterministic:   true,
				SQLiteAffinity:  "text",
				CompareStrategy: driver.CompareSQLiteBinaryTypeof,
				KeyComparable:   true,
			},
		},
	}
	src := changeset.SourceRef{ConnectionID: "sqlite-1", Target: changeset.ResolvedTarget{Engine: dto.EngineSQLite, Database: "main", Table: "items"}}

	t.Run("existing REAL 0.1 restores as float64 in WHERE while updating to 0.25", func(t *testing.T) {
		plan, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "sqlite-1",
			Database:     "main",
			Table:        "items",
			Updates: []dto.GridRowUpdate{
				{
					RowKey:    "r1",
					KeyValues: map[string]dto.OriginalCellValue{"id": {Kind: dto.KindInt, Text: "1", StorageClass: "integer"}},
					OriginalValues: map[string]dto.OriginalCellValue{
						"ratio": {Kind: dto.KindFloat, Text: "0.1", StorageClass: "real"},
					},
					NewValues: map[string]dto.NewCellValue{
						"ratio": {Mode: dto.CellInputLiteral, Text: "0.25"},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("expected existing REAL 0.1 update to succeed, got: %v", err)
		}
		args := plan.Steps[0].Stmt.Args
		// SET ratio = 0.25 (float64), WHERE id = 1 (int64) AND typeof = "integer" AND ratio = 0.1 (float64) AND typeof = "real"
		if len(args) != 5 {
			t.Fatalf("expected 5 args, got %#v", args)
		}
		if args[0] != float64(0.25) || args[1] != int64(1) || args[2] != "integer" || args[3] != float64(0.1) || args[4] != "real" {
			t.Fatalf("unexpected bound SQLite args: %#v", args)
		}
	})

	t.Run("NUMERIC column allows non-numeric DATETIME and JSON text verbatim as string", func(t *testing.T) {
		plan, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "sqlite-1",
			Database:     "main",
			Table:        "items",
			Inserts: []dto.GridRowInsert{
				{
					RowKey: "ins-dt",
					Values: map[string]dto.NewCellValue{
						"created_at": {Mode: dto.CellInputLiteral, Text: "2026-11-27T12:00:00Z"},
						"amt":        {Mode: dto.CellInputLiteral, Text: `{"a":1}`},
						"note":       {Mode: dto.CellInputLiteral, Text: "1234567890123456789012345678.1234567890"},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("expected non-numeric text on NUMERIC and arbitrary precision on TEXT to succeed, got: %v", err)
		}
		args := plan.Steps[0].Stmt.Args
		if len(args) != 3 || args[0] != `{"a":1}` || args[1] != "2026-11-27T12:00:00Z" || args[2] != "1234567890123456789012345678.1234567890" {
			t.Fatalf("unexpected args: %#v", args)
		}
	})

	t.Run("NUMERIC and REAL reject rounded float64 literals like 1e23 and 0.1 while accepting exact binary64", func(t *testing.T) {
		for _, bad := range []string{"1e23", "0.1", "12.34", "1234567890123456789012345678.1234567890"} {
			_, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
				ConnectionID: "sqlite-1",
				Database:     "main",
				Table:        "items",
				Inserts: []dto.GridRowInsert{
					{RowKey: "ins-prec", Values: map[string]dto.NewCellValue{"amt": {Mode: dto.CellInputLiteral, Text: bad}}},
				},
			})
			assertAppErrorCode(t, err, dto.ErrPrecisionUnsupported)
		}

		// Exact dyadic / IEEE-754 float64 values (0.25, 12.75, 1e22) succeed.
		plan, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "sqlite-1",
			Database:     "main",
			Table:        "items",
			Inserts: []dto.GridRowInsert{
				{RowKey: "ins-exact", Values: map[string]dto.NewCellValue{
					"ratio": {Mode: dto.CellInputLiteral, Text: "12.75"},
					"amt":   {Mode: dto.CellInputLiteral, Text: "1e22"},
				}},
			},
		})
		if err != nil || len(plan.Steps) != 1 {
			t.Fatalf("expected exact binary64 literals to succeed, got err=%v", err)
		}

		// Restoring existing REAL 0.1 back to "0.1" strips cleanly as no-op (0 UPDATEs).
		noopPlan, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "sqlite-1",
			Database:     "main",
			Table:        "items",
			Updates: []dto.GridRowUpdate{{
				RowKey:         "r-noop-real",
				KeyValues:      map[string]dto.OriginalCellValue{"id": {Kind: dto.KindInt, Text: "1", StorageClass: "integer"}},
				OriginalValues: map[string]dto.OriginalCellValue{"ratio": {Kind: dto.KindFloat, Text: "0.1", StorageClass: "real"}},
				NewValues:      map[string]dto.NewCellValue{"ratio": {Mode: dto.CellInputLiteral, Text: "0.1"}},
			}},
		})
		if err != nil || len(noopPlan.Steps) != 0 {
			t.Fatalf("expected restoring existing REAL 0.1 to strip as no-op, got len=%d err=%v", len(noopPlan.Steps), err)
		}

		// Non-STRICT INTEGER column can legitimately store REAL 0.1 on disk:
		// restoring it back to "0.1" MUST strip as no-op (allowing sibling edits in the same batch),
		// while inserting/updating a NEW "0.1" into INTEGER is still rejected.
		wsIntReal := ws
		wsIntReal.Columns = append([]driver.WriteColumn(nil), ws.Columns...)
		wsIntReal.Columns = append(wsIntReal.Columns, driver.WriteColumn{
			Detail:          dto.ColumnDetail{Name: "score", DataType: "INTEGER", ColumnType: "INTEGER", Nullable: true, Insertable: true, Updatable: true, OrdinalPos: 6},
			NativeType:      "INTEGER",
			Deterministic:   true,
			SQLiteAffinity:  "integer",
			CompareStrategy: driver.CompareSQLiteBinaryTypeof,
			KeyComparable:   true,
		})
		siblingPlan, _, err := changeset.BuildChangePlan(src, wsIntReal, dto.PreviewGridChangesRequest{
			ConnectionID: "sqlite-1",
			Database:     "main",
			Table:        "items",
			Updates: []dto.GridRowUpdate{{
				RowKey:    "r-int-real-noop",
				KeyValues: map[string]dto.OriginalCellValue{"id": {Kind: dto.KindInt, Text: "1", StorageClass: "integer"}},
				OriginalValues: map[string]dto.OriginalCellValue{
					"score": {Kind: dto.KindFloat, Text: "0.1", StorageClass: "real"},
					"note":  {Kind: dto.KindString, Text: "Old", StorageClass: "text"},
				},
				NewValues: map[string]dto.NewCellValue{
					"score": {Mode: dto.CellInputLiteral, Text: "0.1"},
					"note":  {Mode: dto.CellInputLiteral, Text: "New"},
				},
			}},
		})
		if err != nil {
			t.Fatalf("expected restoring existing REAL 0.1 on INTEGER column alongside sibling edit to succeed, got err=%v", err)
		}
		if len(siblingPlan.Steps) != 1 {
			t.Fatalf("expected 1 UPDATE step for sibling edit, got %d", len(siblingPlan.Steps))
		}
		if strings.Contains(siblingPlan.Steps[0].Stmt.SQL, `"score"`) {
			t.Fatalf("restored REAL 0.1 on INTEGER column must be stripped from UPDATE SQL, got: %s", siblingPlan.Steps[0].Stmt.SQL)
		}

		// Still reject a NEW fractional value "0.1" written to an INTEGER column.
		_, _, err = changeset.BuildChangePlan(src, wsIntReal, dto.PreviewGridChangesRequest{
			ConnectionID: "sqlite-1",
			Database:     "main",
			Table:        "items",
			Inserts: []dto.GridRowInsert{{
				RowKey: "ins-int-bad",
				Values: map[string]dto.NewCellValue{"score": {Mode: dto.CellInputLiteral, Text: "0.1"}},
			}},
		})
		assertAppErrorCode(t, err, dto.ErrPrecisionUnsupported)
	})

	t.Run("SQLite STRICT ANY column preserves leading zeros as TEXT while non-STRICT ANY coerces to integer", func(t *testing.T) {
		wsStrict := ws
		wsStrict.Strict = true
		wsStrict.Columns = append([]driver.WriteColumn(nil), ws.Columns...)
		wsStrict.Columns = append(wsStrict.Columns, driver.WriteColumn{
			Detail:          dto.ColumnDetail{Name: "flex", DataType: "ANY", ColumnType: "ANY", Nullable: true, Insertable: true, Updatable: true, OrdinalPos: 6},
			NativeType:      "ANY",
			Deterministic:   true,
			SQLiteAffinity:  "any",
			CompareStrategy: driver.CompareSQLiteBinaryTypeof,
			KeyComparable:   true,
		})
		pStrict, _, err := changeset.BuildChangePlan(src, wsStrict, dto.PreviewGridChangesRequest{
			ConnectionID: "sqlite-1", Database: "main", Table: "items",
			Inserts: []dto.GridRowInsert{{RowKey: "strict-any", Values: map[string]dto.NewCellValue{"flex": {Mode: dto.CellInputLiteral, Text: "000123"}}}},
		})
		if err != nil {
			t.Fatalf("strict ANY plan failed: %v", err)
		}
		if got, ok := pStrict.Steps[0].Stmt.Args[0].(string); !ok || got != "000123" {
			t.Fatalf("STRICT ANY must preserve '000123' as string, got %T(%#v)", pStrict.Steps[0].Stmt.Args[0], pStrict.Steps[0].Stmt.Args[0])
		}

		wsRegular := ws
		wsRegular.Strict = false
		wsRegular.Columns = append([]driver.WriteColumn(nil), ws.Columns...)
		wsRegular.Columns = append(wsRegular.Columns, driver.WriteColumn{
			Detail:          dto.ColumnDetail{Name: "flex", DataType: "ANY", ColumnType: "ANY", Nullable: true, Insertable: true, Updatable: true, OrdinalPos: 6},
			NativeType:      "ANY",
			Deterministic:   true,
			SQLiteAffinity:  "numeric",
			CompareStrategy: driver.CompareSQLiteBinaryTypeof,
			KeyComparable:   true,
		})
		pReg, _, err := changeset.BuildChangePlan(src, wsRegular, dto.PreviewGridChangesRequest{
			ConnectionID: "sqlite-1", Database: "main", Table: "items",
			Inserts: []dto.GridRowInsert{{RowKey: "reg-any", Values: map[string]dto.NewCellValue{"flex": {Mode: dto.CellInputLiteral, Text: "000123"}}}},
		})
		if err != nil {
			t.Fatalf("regular ANY plan failed: %v", err)
		}
		if got, ok := pReg.Steps[0].Stmt.Args[0].(int64); !ok || got != 123 {
			t.Fatalf("regular non-STRICT ANY should bind '000123' as int64(123), got %T(%#v)", pReg.Steps[0].Stmt.Args[0], pReg.Steps[0].Stmt.Args[0])
		}
	})

	t.Run("INTEGER column rejects decimal and int64 overflow with ErrPrecisionUnsupported", func(t *testing.T) {
		for _, bad := range []string{"1.5", "9223372036854775808"} {
			_, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
				ConnectionID: "sqlite-1",
				Database:     "main",
				Table:        "items",
				Inserts: []dto.GridRowInsert{
					{RowKey: "ins-int", Values: map[string]dto.NewCellValue{"id": {Mode: dto.CellInputLiteral, Text: bad}}},
				},
			})
			assertAppErrorCode(t, err, dto.ErrPrecisionUnsupported)
		}
	})
}

func TestPlanStoreStateMachinePayloadDropAndRetention(t *testing.T) {
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	store := changeset.NewPlanStore(changeset.PlanStoreOptions{
		ReadyTTL:            5 * time.Minute,
		MaxReadyPlans:       3,
		MaxFinishedRetained: 3,
		Now:                 func() time.Time { return now },
	})

	makePlan := func(connID string) *changeset.ChangePlan {
		return &changeset.ChangePlan{
			Source: changeset.SourceRef{ConnectionID: connID},
			Steps:  []changeset.ChangeStep{{Index: 0, Op: changeset.OpInsert, RowKey: "i1"}},
		}
	}

	// 1. Ready -> Preparing -> ReleaseToReady -> Preparing -> Executing -> Finished(Committed)
	id1 := store.Create(makePlan("c1"))
	p, snap, err := store.ReserveForApply(id1, "c1")
	if err != nil || snap != nil || p == nil {
		t.Fatalf("expected initial reserve to succeed, got p=%v snap=%v err=%v", p, snap, err)
	}
	// Concurrent re-entry during Preparing must fail.
	_, _, err = store.ReserveForApply(id1, "c1")
	assertAppErrorCode(t, err, dto.ErrInvalidArgument)

	// Whitelisted SSH retry releases back to Ready.
	if !store.ReleaseToReady(id1) {
		t.Fatalf("expected ReleaseToReady from Preparing to succeed")
	}
	p, snap, err = store.ReserveForApply(id1, "c1")
	if err != nil || snap != nil || p == nil {
		t.Fatalf("expected second reserve after ReleaseToReady to succeed")
	}
	if err := store.MarkExecuting(id1); err != nil {
		t.Fatalf("MarkExecuting failed: %v", err)
	}
	// ReleaseToReady after Executing MUST be a no-op!
	if store.ReleaseToReady(id1) {
		t.Fatalf("ReleaseToReady MUST NOT succeed once Executing")
	}
	// Concurrent re-entry during Executing must fail.
	_, _, err = store.ReserveForApply(id1, "c1")
	assertAppErrorCode(t, err, dto.ErrInvalidArgument)

	// MarkFinished drops heavy plan payload immediately.
	if !store.MarkFinished(id1, dto.GridApplyCommitted, changeset.ExecutionSummary{AppliedCount: 1, InsertCount: 1}, nil) {
		t.Fatalf("MarkFinished failed")
	}
	st, hasPayload, exists := store.StateOf(id1)
	if !exists || st != changeset.PlanFinished || hasPayload {
		t.Fatalf("expected Finished with payload dropped (hasPayload=false), got st=%s hasPayload=%v exists=%v", st, hasPayload, exists)
	}

	// Subsequent MarkFinished (e.g. from defer cleanup) MUST NOT overwrite existing Finished outcome!
	if store.MarkFinished(id1, dto.GridApplyNotCommitted, changeset.ExecutionSummary{}, dto.NewAppError(dto.ErrTxAborted, "cleanup", "")) {
		t.Fatalf("MarkFinished MUST NOT overwrite existing Finished outcome")
	}
	_, snap, err = store.ReserveForApply(id1, "c1")
	if err != nil || snap == nil || snap.Outcome != dto.GridApplyCommitted || snap.Summary.AppliedCount != 1 {
		t.Fatalf("expected cached Committed snapshot, got snap=%+v err=%v", snap, err)
	}

	// 2. Preparing and Executing entries are NEVER evicted; Finished cap prioritizes Unknown retention.
	now = now.Add(time.Second)
	idUnknown := store.Create(makePlan("c1"))
	_, _, _ = store.ReserveForApply(idUnknown, "c1")
	_ = store.MarkExecuting(idUnknown)
	store.MarkFinished(idUnknown, dto.GridApplyUnknown, changeset.ExecutionSummary{}, &dto.AppError{
		Code: dto.ErrCommitOutcomeUnknown, RetrySafe: false,
	})

	// Keep one plan pinned in Preparing while creating overflow Ready plans.
	idPinned := store.Create(makePlan("c1"))
	_, _, _ = store.ReserveForApply(idPinned, "c1")
	for i := 0; i < 5; i++ {
		now = now.Add(time.Second)
		store.Create(makePlan("c1"))
	}
	if st, _, ok := store.StateOf(idPinned); !ok || st != changeset.PlanPreparing {
		t.Fatalf("Preparing plan must never be evicted, got st=%s ok=%v", st, ok)
	}

	// Push 3 more Finished(NotCommitted) records (cap=3): id1 & oldest NotCommitted should evict BEFORE idUnknown!
	for i := 0; i < 3; i++ {
		now = now.Add(time.Second)
		fid := store.Create(makePlan("c1"))
		_, _, _ = store.ReserveForApply(fid, "c1")
		store.MarkFinished(fid, dto.GridApplyNotCommitted, changeset.ExecutionSummary{}, dto.NewAppError(dto.ErrTxAborted, "fail", ""))
	}
	_, snapU, errU := store.ReserveForApply(idUnknown, "c1")
	if errU != nil || snapU == nil || snapU.Outcome != dto.GridApplyUnknown {
		t.Fatalf("Unknown terminal record must be prioritized over non-Unknown on eviction, got snap=%+v err=%v", snapU, errU)
	}
	// Evicted id1 must return ErrPlanInvalidated and NEVER re-execute.
	_, _, errEvicted := store.ReserveForApply(id1, "c1")
	assertAppErrorCode(t, errEvicted, dto.ErrPlanInvalidated)
}

func TestExecutePlanWithFakeFaultInjection(t *testing.T) {
	ws := sampleMySQLWriteSchema()
	src := sampleSourceRef()
	plan, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
		ConnectionID: "conn-1",
		Database:     "demo",
		Table:        "users",
		Updates: []dto.GridRowUpdate{
			{
				RowKey:         "row-42",
				KeyValues:      map[string]dto.OriginalCellValue{"id": {Kind: dto.KindInt, Text: "42"}},
				OriginalValues: map[string]dto.OriginalCellValue{"name": {Kind: dto.KindString, Text: "Alice"}},
				NewValues:      map[string]dto.NewCellValue{"name": {Mode: dto.CellInputLiteral, Text: "Bob"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	t.Run("AffectedRows == 0 triggers ErrConcurrencyConflict with ConflictRowKey", func(t *testing.T) {
		opener := fake.NewOpener(fake.FaultBehavior{
			WriteSchema:      &ws,
			ExecAffectedRows: []int64{0},
		})
		conn, _ := opener.Open(context.Background(), driver.ResolvedConfig{})
		tx, err := conn.(driver.TableWriteStarter).BeginTableWrite(context.Background(), driver.WriteTarget{Database: "demo", Table: "users"}, false)
		if err != nil {
			t.Fatalf("BeginTableWrite: %v", err)
		}
		res := changeset.ExecutePlan(context.Background(), tx, plan, nil)
		if res.Outcome != dto.GridApplyNotCommitted || res.Summary.ConflictRowKey != "row-42" {
			t.Fatalf("expected NotCommitted + ConflictRowKey=row-42, got %+v", res)
		}
		assertAppErrorCode(t, res.Err, dto.ErrConcurrencyConflict)
	})

	t.Run("AffectedRows > 1 triggers ErrRowNotIdentifiable", func(t *testing.T) {
		opener := fake.NewOpener(fake.FaultBehavior{
			WriteSchema:      &ws,
			ExecAffectedRows: []int64{2},
		})
		conn, _ := opener.Open(context.Background(), driver.ResolvedConfig{})
		tx, _ := conn.(driver.TableWriteStarter).BeginTableWrite(context.Background(), driver.WriteTarget{Database: "demo", Table: "users"}, false)
		res := changeset.ExecutePlan(context.Background(), tx, plan, nil)
		if res.Outcome != dto.GridApplyNotCommitted {
			t.Fatalf("expected NotCommitted, got %+v", res)
		}
		assertAppErrorCode(t, res.Err, dto.ErrRowNotIdentifiable)
	})

	t.Run("single-row INSERT affecting 0 rows (e.g. BEFORE INSERT RETURN NULL) rolls back batch", func(t *testing.T) {
		insPlan, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "conn-1",
			Database:     "demo",
			Table:        "users",
			Inserts: []dto.GridRowInsert{
				{RowKey: "ins-skip", Values: map[string]dto.NewCellValue{"name": {Mode: dto.CellInputLiteral, Text: "Skipped"}}},
			},
		})
		if err != nil {
			t.Fatalf("build insert plan: %v", err)
		}
		opener := fake.NewOpener(fake.FaultBehavior{
			WriteSchema:      &ws,
			ExecAffectedRows: []int64{0},
		})
		conn, _ := opener.Open(context.Background(), driver.ResolvedConfig{})
		tx, _ := conn.(driver.TableWriteStarter).BeginTableWrite(context.Background(), driver.WriteTarget{Database: "demo", Table: "users"}, false)
		res := changeset.ExecutePlan(context.Background(), tx, insPlan, nil)
		if res.Outcome != dto.GridApplyNotCommitted || res.Summary.ConflictRowKey != "ins-skip" {
			t.Fatalf("expected NotCommitted + ConflictRowKey=ins-skip when INSERT affects 0 rows, got %+v", res)
		}
		assertAppErrorCode(t, res.Err, dto.ErrTxAborted)
	})

	t.Run("in-tx column type change triggers ErrSchemaChanged before executing", func(t *testing.T) {
		mutated := sampleMySQLWriteSchema()
		mutated.Columns[2].Detail.ColumnType = "decimal(38,2)"
		opener := fake.NewOpener(fake.FaultBehavior{
			MutateSchemaBeforeBeginWrite: func() driver.WriteSchema { return mutated },
		})
		conn, _ := opener.Open(context.Background(), driver.ResolvedConfig{})
		tx, _ := conn.(driver.TableWriteStarter).BeginTableWrite(context.Background(), driver.WriteTarget{Database: "demo", Table: "users"}, false)
		markCalled := false
		res := changeset.ExecutePlan(context.Background(), tx, plan, func() error {
			markCalled = true
			return nil
		})
		if markCalled {
			t.Fatalf("markExecuting MUST NOT be called when SchemaFingerprint mismatches")
		}
		assertAppErrorCode(t, res.Err, dto.ErrSchemaChanged)
		if len(opener.ExecutedStatements()) != 0 {
			t.Fatalf("expected 0 SQL statements executed on schema mismatch, got %d", len(opener.ExecutedStatements()))
		}
	})

	t.Run("in-tx engine altered to MyISAM triggers ErrBatchNonTx", func(t *testing.T) {
		mutated := sampleMySQLWriteSchema()
		mutated.StorageEngine = "MyISAM"
		mutated.Editability.BatchTransactional = false
		opener := fake.NewOpener(fake.FaultBehavior{
			MutateSchemaBeforeBeginWrite: func() driver.WriteSchema { return mutated },
		})
		conn, _ := opener.Open(context.Background(), driver.ResolvedConfig{})
		tx, _ := conn.(driver.TableWriteStarter).BeginTableWrite(context.Background(), driver.WriteTarget{Database: "demo", Table: "users"}, false)
		res := changeset.ExecutePlan(context.Background(), tx, plan, nil)
		assertAppErrorCode(t, res.Err, dto.ErrBatchNonTx)
	})
}

func TestPostgreSQLNonDeterministicBPCharPredicate(t *testing.T) {
	ws := driver.WriteSchema{
		Dialect:  dto.EnginePostgres,
		Database: "sqlm_demo",
		Schema:   "public",
		Table:    "t_bp",
		Editability: dto.TableEditability{
			CanWrite:           true,
			RowIdentity:        dto.RowIdentityPrimaryKey,
			KeyColumns:         []string{"k_bp"},
			BatchTransactional: true,
		},
		Columns: []driver.WriteColumn{
			{
				Detail:          dto.ColumnDetail{Name: "k_bp", DataType: "character(4)", ColumnType: "character(4)", IsPrimaryKey: true, Insertable: true, Updatable: true, OrdinalPos: 1},
				NativeType:      "character(4)",
				Collation:       "ci_icu",
				Deterministic:   false,
				CompareStrategy: driver.ComparePGCCollationBPChar,
				KeyComparable:   true,
			},
			{
				Detail:          dto.ColumnDetail{Name: "v_bp", DataType: "character(4)", ColumnType: "character(4)", Nullable: true, Insertable: true, Updatable: true, OrdinalPos: 2},
				NativeType:      "character(4)",
				Collation:       "ci_icu",
				Deterministic:   false,
				CompareStrategy: driver.ComparePGCCollationBPChar,
				KeyComparable:   true,
			},
			{
				Detail:          dto.ColumnDetail{Name: "v_txt", DataType: "text", ColumnType: "text", Nullable: true, Insertable: true, Updatable: true, OrdinalPos: 3},
				NativeType:      "text",
				Collation:       "ci_icu",
				Deterministic:   false,
				CompareStrategy: driver.ComparePGCCollation,
				KeyComparable:   true,
			},
		},
	}
	src := changeset.SourceRef{ConnectionID: "pg-1", Target: changeset.ResolvedTarget{Engine: dto.EnginePostgres, Database: "sqlm_demo", Schema: "public", Table: "t_bp"}}

	plan, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
		ConnectionID: "pg-1", Database: "sqlm_demo", Schema: "public", Table: "t_bp",
		Updates: []dto.GridRowUpdate{{
			RowKey:    "bp-1",
			KeyValues: map[string]dto.OriginalCellValue{"k_bp": {Kind: dto.KindString, Text: "K1  "}},
			OriginalValues: map[string]dto.OriginalCellValue{
				"v_bp":  {Kind: dto.KindString, Text: "A   "},
				"v_txt": {Kind: dto.KindString, Text: "Hello "},
			},
			NewValues: map[string]dto.NewCellValue{
				"v_bp":  {Mode: dto.CellInputLiteral, Text: "B"},
				"v_txt": {Mode: dto.CellInputLiteral, Text: "World"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("BuildChangePlan for PG bpchar failed: %v", err)
	}
	gotSQL := plan.Steps[0].Stmt.SQL
	wantKeyPred := `("k_bp" = $3 AND "k_bp" COLLATE "C" = $4::pg_catalog.bpchar)`
	wantColPred := `"v_bp" COLLATE "C" = $5::pg_catalog.bpchar`
	wantTxtPred := `"v_txt"::text COLLATE "C" = $6`
	if !strings.Contains(gotSQL, wantKeyPred) || !strings.Contains(gotSQL, wantColPred) || !strings.Contains(gotSQL, wantTxtPred) {
		t.Fatalf("unexpected PG bpchar/text predicates in SQL:\n%s", gotSQL)
	}
}

func TestPlanStoreReleaseToReadyEnforcesTTLAndCapacity(t *testing.T) {
	t.Run("expired plan returns false on ReleaseToReady and does not reset createdAt", func(t *testing.T) {
		now := time.Date(2026, 11, 28, 8, 0, 0, 0, time.UTC)
		store := changeset.NewPlanStore(changeset.PlanStoreOptions{
			ReadyTTL: 10 * time.Second,
			Now:      func() time.Time { return now },
		})
		id := store.Create(&changeset.ChangePlan{Source: changeset.SourceRef{ConnectionID: "c1"}})

		// Reserve just before TTL expires.
		now = now.Add(9 * time.Second)
		if _, _, err := store.ReserveForApply(id, "c1"); err != nil {
			t.Fatalf("ReserveForApply before TTL failed: %v", err)
		}

		// Dial fails AFTER original createdAt + ReadyTTL.
		now = now.Add(2 * time.Second)
		if ok := store.ReleaseToReady(id); ok {
			t.Fatalf("expected ReleaseToReady to return false for plan expired during Preparing")
		}
		if _, _, exists := store.StateOf(id); exists {
			t.Fatalf("expected expired plan deleted from store on ReleaseToReady")
		}
	})

	t.Run("ReleaseToReady enforces MaxReadyPlans cap", func(t *testing.T) {
		now := time.Date(2026, 11, 28, 8, 0, 0, 0, time.UTC)
		store := changeset.NewPlanStore(changeset.PlanStoreOptions{
			ReadyTTL:      time.Minute,
			MaxReadyPlans: 1,
			Now:           func() time.Time { return now },
		})
		idA := store.Create(&changeset.ChangePlan{Source: changeset.SourceRef{ConnectionID: "c1"}})
		if _, _, err := store.ReserveForApply(idA, "c1"); err != nil {
			t.Fatalf("ReserveForApply A: %v", err)
		}

		// Create plan B while A is Preparing.
		now = now.Add(time.Second)
		idB := store.Create(&changeset.ChangePlan{Source: changeset.SourceRef{ConnectionID: "c1"}})

		// Release A back to Ready at t=2 (newer updatedAt than idle B created at t=1):
		// LRU eviction (updatedAt first) MUST keep actively-retrying A and evict idle B!
		now = now.Add(time.Second)
		okA := store.ReleaseToReady(idA)
		stA, _, exA := store.StateOf(idA)
		stB, _, exB := store.StateOf(idB)

		if !okA || !exA || stA != changeset.PlanReady || exB {
			t.Fatalf("expected LRU eviction to keep more recently updated plan A (okA=true, PlanReady) and evict idle B, got okA=%v A(ex=%v st=%s) B(ex=%v st=%s)",
				okA, exA, stA, exB, stB)
		}

		// Tie-breaker: when updatedAt is equal, older createdAt (A) is evicted and newer B survives (okA=false)!
		if _, _, err := store.ReserveForApply(idA, "c1"); err != nil {
			t.Fatalf("ReserveForApply A second time: %v", err)
		}
		now = now.Add(time.Second)
		idC := store.Create(&changeset.ChangePlan{Source: changeset.SourceRef{ConnectionID: "c1"}})
		// Same timestamp `now` -> A.updatedAt == C.updatedAt, A.createdAt < C.createdAt -> A MUST be evicted.
		okA2 := store.ReleaseToReady(idA)
		stA2, _, exA2 := store.StateOf(idA)
		stC, _, exC := store.StateOf(idC)
		if okA2 || exA2 || !exC || stC != changeset.PlanReady {
			t.Fatalf("expected createdAt tie-breaker to evict older plan A (okA2=false, exA2=false) and keep newer C (PlanReady), got okA2=%v A(ex=%v st=%s) C(ex=%v st=%s)",
				okA2, exA2, stA2, exC, stC)
		}
	})
}

func TestSQLiteFormFeedAndVerticalTabNumericWhitespace(t *testing.T) {
	ws := driver.WriteSchema{
		Dialect:  dto.EngineSQLite,
		Database: "main",
		Table:    "t_ws",
		Editability: dto.TableEditability{
			CanWrite:           true,
			RowIdentity:        dto.RowIdentityPrimaryKey,
			KeyColumns:         []string{"id"},
			BatchTransactional: true,
		},
		Columns: []driver.WriteColumn{
			{
				Detail:          dto.ColumnDetail{Name: "id", DataType: "INTEGER", ColumnType: "INTEGER", IsPrimaryKey: true, Insertable: true, Updatable: true, OrdinalPos: 1},
				NativeType:      "INTEGER",
				Deterministic:   true,
				SQLiteAffinity:  "integer",
				CompareStrategy: driver.CompareSQLiteBinaryTypeof,
				KeyComparable:   true,
			},
			{
				Detail:          dto.ColumnDetail{Name: "num_col", DataType: "NUMERIC", ColumnType: "NUMERIC", Nullable: true, Insertable: true, Updatable: true, OrdinalPos: 2},
				NativeType:      "NUMERIC",
				Deterministic:   true,
				SQLiteAffinity:  "numeric",
				CompareStrategy: driver.CompareSQLiteBinaryTypeof,
				KeyComparable:   true,
			},
			{
				Detail:          dto.ColumnDetail{Name: "real_col", DataType: "REAL", ColumnType: "REAL", Nullable: true, Insertable: true, Updatable: true, OrdinalPos: 3},
				NativeType:      "REAL",
				Deterministic:   true,
				SQLiteAffinity:  "real",
				CompareStrategy: driver.CompareSQLiteBinaryTypeof,
				KeyComparable:   false,
			},
		},
	}
	src := changeset.SourceRef{ConnectionID: "sq-1", Target: changeset.ResolvedTarget{Engine: dto.EngineSQLite, Database: "main", Table: "t_ws"}}

	for _, bad := range []string{"\f0.1", "\v0.1", "0.1\f", "\v1e23\f"} {
		_, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
			ConnectionID: "sq-1", Database: "main", Table: "t_ws",
			Inserts: []dto.GridRowInsert{{
				RowKey: "bad-ws",
				Values: map[string]dto.NewCellValue{
					"id":      {Mode: dto.CellInputLiteral, Text: "1"},
					"num_col": {Mode: dto.CellInputLiteral, Text: bad},
				},
			}},
		})
		var ae *dto.AppError
		if !errors.As(err, &ae) || ae.Code != dto.ErrPrecisionUnsupported {
			t.Fatalf("expected ErrPrecisionUnsupported for NUMERIC %q, got %v", bad, err)
		}
	}

	plan, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
		ConnectionID: "sq-1", Database: "main", Table: "t_ws",
		Inserts: []dto.GridRowInsert{{
			RowKey: "good-ws",
			Values: map[string]dto.NewCellValue{
				"id":       {Mode: dto.CellInputLiteral, Text: "\f7\v"},
				"num_col":  {Mode: dto.CellInputLiteral, Text: "\v42\f"},
				"real_col": {Mode: dto.CellInputLiteral, Text: "\f0.5\v"},
			},
		}},
	})
	if err != nil || len(plan.Steps) != 1 {
		t.Fatalf("expected lossless SQLite \\f/\\v literals to bind cleanly, got err=%v", err)
	}
	args := plan.Steps[0].Stmt.Args
	if len(args) != 3 {
		t.Fatalf("expected 3 args, got %d (%v)", len(args), args)
	}
	// Column order in BuildChangePlan follows ws.Columns ordinal order: id, num_col, real_col.
	if v, ok := args[0].(int64); !ok || v != 7 {
		t.Fatalf("expected id=int64(7), got %T(%v)", args[0], args[0])
	}
	if v, ok := args[1].(int64); !ok || v != 42 {
		t.Fatalf("expected num_col=int64(42), got %T(%v)", args[1], args[1])
	}
	if v, ok := args[2].(float64); !ok || v != 0.5 {
		t.Fatalf("expected real_col=float64(0.5), got %T(%v)", args[2], args[2])
	}

	// SQLite NUMERIC affinity: exact integral literal "42.0" binds as int64(42) with "integer" storage class
	// (so editing existing integer 42 -> "42.0" is detected as a no-op), while "0.25" stays float64/"real".
	noopPlan, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
		ConnectionID: "sq-1", Database: "main", Table: "t_ws",
		Updates: []dto.GridRowUpdate{{
			RowKey:         "noop-numeric-int",
			KeyValues:      map[string]dto.OriginalCellValue{"id": {Kind: dto.KindInt, Text: "7", StorageClass: "integer"}},
			OriginalValues: map[string]dto.OriginalCellValue{"num_col": {Kind: dto.KindInt, Text: "42", StorageClass: "integer"}},
			NewValues:      map[string]dto.NewCellValue{"num_col": {Mode: dto.CellInputLiteral, Text: "42.0"}},
		}},
	})
	if err != nil {
		t.Fatalf("BuildChangePlan NUMERIC 42 -> 42.0: %v", err)
	}
	if len(noopPlan.Steps) != 0 {
		t.Fatalf("expected NUMERIC 42 (integer) -> 42.0 to be classified as no-op, got %d steps", len(noopPlan.Steps))
	}

	fracPlan, _, err := changeset.BuildChangePlan(src, ws, dto.PreviewGridChangesRequest{
		ConnectionID: "sq-1", Database: "main", Table: "t_ws",
		Inserts: []dto.GridRowInsert{{
			RowKey: "ins-numeric-frac",
			Values: map[string]dto.NewCellValue{
				"id":      {Mode: dto.CellInputLiteral, Text: "8"},
				"num_col": {Mode: dto.CellInputLiteral, Text: "0.25"},
			},
		}},
	})
	if err != nil || len(fracPlan.Steps) != 1 {
		t.Fatalf("BuildChangePlan NUMERIC 0.25: %v", err)
	}
	if v, ok := fracPlan.Steps[0].Stmt.Args[1].(float64); !ok || v != 0.25 {
		t.Fatalf("expected NUMERIC 0.25 to bind as float64(0.25), got %T(%v)", fracPlan.Steps[0].Stmt.Args[1], fracPlan.Steps[0].Stmt.Args[1])
	}
}

func TestSchemaFingerprintDetectsTableDefinitionAndGenerationExpressionChanges(t *testing.T) {
	ws1 := driver.WriteSchema{
		Dialect:         dto.EngineSQLite,
		Database:        "main",
		Table:           "t_gen",
		TableDefinition: "CREATE TABLE t_gen (id INTEGER PRIMARY KEY, qty INTEGER, g INTEGER GENERATED ALWAYS AS (qty * 2) VIRTUAL)",
	}
	ws2 := ws1
	ws2.TableDefinition = "CREATE TABLE t_gen (id INTEGER PRIMARY KEY, qty INTEGER, g INTEGER GENERATED ALWAYS AS (qty * 3) VIRTUAL)"

	if changeset.ComputeSchemaFingerprint(ws1) == changeset.ComputeSchemaFingerprint(ws2) {
		t.Fatalf("expected SchemaFingerprint to change when SQLite generated expression TableDefinition changes")
	}

	my1 := driver.WriteSchema{
		Dialect:  dto.EngineMySQL,
		Database: "app",
		Table:    "t_gen",
		Columns: []driver.WriteColumn{
			{
				Detail:               dto.ColumnDetail{Name: "g", DataType: "int", ColumnType: "int", GeneratedKind: "virtual", OrdinalPos: 1},
				NativeType:           "int",
				GenerationExpression: "(`qty` * 2)",
			},
		},
	}
	my2 := my1
	my2.Columns = append([]driver.WriteColumn(nil), my1.Columns...)
	my2.Columns[0].GenerationExpression = "(`qty` * 3)"
	if changeset.ComputeSchemaFingerprint(my1) == changeset.ComputeSchemaFingerprint(my2) {
		t.Fatalf("expected SchemaFingerprint to change when MySQL GenerationExpression changes")
	}
}

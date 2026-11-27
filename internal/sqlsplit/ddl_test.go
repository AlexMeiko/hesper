package sqlsplit

import "testing"

func TestInspectDDL(t *testing.T) {
	tests := []struct {
		name         string
		dialect      Dialect
		sql          string
		wantScope    DDLScope
		wantVerb     string
		wantName     string
		wantDB       string
		wantSchema   string
		wantExtraDBs []string
		wantIsDropDB bool
		wantHasForce bool
	}{
		{
			name:         "pg_drop_database_unquoted_folds_ascii_lowercase",
			dialect:      DialectPostgres,
			sql:          "-- cleanup\nDROP DATABASE Blue_M2_2026;",
			wantScope:    DDLScopeDatabase,
			wantVerb:     "DROP",
			wantName:     "blue_m2_2026",
			wantIsDropDB: true,
		},
		{
			name:         "pg_drop_database_quoted_preserves_case_and_escaped_quotes",
			dialect:      DialectPostgres,
			sql:          `/* nested /* comment */ */ DROP DATABASE IF EXISTS "Blue""M2" WITH (FORCE);`,
			wantScope:    DDLScopeDatabase,
			wantVerb:     "DROP",
			wantName:     `Blue"M2`,
			wantIsDropDB: true,
			wantHasForce: true,
		},
		{
			name:         "pg_drop_database_quoted_unicode_is_exact",
			dialect:      DialectPostgres,
			sql:          `DROP DATABASE "Ärger_测试";`,
			wantScope:    DDLScopeDatabase,
			wantVerb:     "DROP",
			wantName:     "Ärger_测试",
			wantIsDropDB: true,
		},
		{
			name:         "pg_drop_database_shorthand_force_option",
			dialect:      DialectPostgres,
			sql:          `DROP DATABASE app_tmp (FORCE);`,
			wantScope:    DDLScopeDatabase,
			wantVerb:     "DROP",
			wantName:     "app_tmp",
			wantIsDropDB: true,
			wantHasForce: true,
		},
		{
			name:         "pg_drop_database_force_false_rejected_by_pg_grammar",
			dialect:      DialectPostgres,
			sql:          `DROP DATABASE app_tmp WITH (FORCE FALSE);`,
			wantScope:    DDLScopeDatabase,
			wantVerb:     "DROP",
			wantName:     "",
			wantIsDropDB: true,
			wantHasForce: false,
		},
		{
			name:         "mysql_drop_database_22_cjk_chars_66_bytes_is_valid",
			dialect:      DialectMySQL,
			sql:          "DROP DATABASE `一二三四五六七八九十一二三四五六七八九十一二`;",
			wantScope:    DDLScopeDatabase,
			wantVerb:     "DROP",
			wantName:     "一二三四五六七八九十一二三四五六七八九十一二",
			wantIsDropDB: true,
		},
		{
			name:         "mysql_drop_database_65_chars_rejected",
			dialect:      DialectMySQL,
			sql:          "DROP DATABASE `aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`;",
			wantScope:    DDLScopeDatabase,
			wantVerb:     "DROP",
			wantName:     "",
			wantIsDropDB: true,
		},
		{
			name:         "pg_drop_database_unicode_escape_is_ambiguous_never_u",
			dialect:      DialectPostgres,
			sql:          `DROP DATABASE U&"foo\0062";`,
			wantScope:    DDLScopeDatabase,
			wantVerb:     "DROP",
			wantName:     "",
			wantIsDropDB: true,
		},
		{
			name:         "pg_drop_database_unicode_escape_with_uescape_is_ambiguous",
			dialect:      DialectPostgres,
			sql:          `DROP DATABASE u&"foo#0062" UESCAPE '#';`,
			wantScope:    DDLScopeDatabase,
			wantVerb:     "DROP",
			wantName:     "",
			wantIsDropDB: true,
		},
		{
			name:         "pg_drop_database_unquoted_non_ascii_is_ambiguous",
			dialect:      DialectPostgres,
			sql:          `DROP DATABASE Ärger;`,
			wantScope:    DDLScopeDatabase,
			wantVerb:     "DROP",
			wantName:     "",
			wantIsDropDB: true,
		},
		{
			name:         "pg_drop_database_unclosed_quote_is_ambiguous",
			dialect:      DialectPostgres,
			sql:          `DROP DATABASE "blue_m2`,
			wantScope:    DDLScopeDatabase,
			wantVerb:     "DROP",
			wantName:     "",
			wantIsDropDB: true,
		},
		{
			name:         "pg_drop_database_unexpected_trailing_token_is_ambiguous",
			dialect:      DialectPostgres,
			sql:          `DROP DATABASE foo bar;`,
			wantScope:    DDLScopeDatabase,
			wantVerb:     "DROP",
			wantName:     "",
			wantIsDropDB: true,
		},
		{
			name:         "pg_drop_database_unknown_option_is_ambiguous",
			dialect:      DialectPostgres,
			sql:          `DROP DATABASE foo WITH (FORCE 'false');`,
			wantScope:    DDLScopeDatabase,
			wantVerb:     "DROP",
			wantName:     "",
			wantIsDropDB: true,
		},
		{
			name:      "pg_create_database",
			dialect:   DialectPostgres,
			sql:       `CREATE DATABASE analytics_db OWNER postgres;`,
			wantScope: DDLScopeDatabase,
			wantVerb:  "CREATE",
			wantName:  "analytics_db",
		},
		{
			name:      "pg_create_schema",
			dialect:   DialectPostgres,
			sql:       `CREATE SCHEMA IF NOT EXISTS "Reporting";`,
			wantScope: DDLScopeSchema,
			wantVerb:  "CREATE",
			wantName:  "Reporting",
		},
		{
			name:       "pg_create_table_with_schema",
			dialect:    DialectPostgres,
			sql:        `CREATE UNLOGGED TABLE IF NOT EXISTS App.Orders (id int);`,
			wantScope:  DDLScopeObject,
			wantVerb:   "CREATE",
			wantName:   "orders",
			wantSchema: "app",
		},
		{
			name:      "pg_temp_table_ignored",
			dialect:   DialectPostgres,
			sql:       `CREATE TEMP TABLE scratch_tmp (id int);`,
			wantScope: DDLScopeNone,
		},
		{
			name:       "pg_materialized_view",
			dialect:    DialectPostgres,
			sql:        `DROP MATERIALIZED VIEW IF EXISTS public.mv_sales;`,
			wantScope:  DDLScopeObject,
			wantVerb:   "DROP",
			wantName:   "mv_sales",
			wantSchema: "public",
		},
		{
			name:         "mysql_drop_database_backtick",
			dialect:      DialectMySQL,
			sql:          "# drop old db\nDROP DATABASE IF EXISTS `Order_DB`;",
			wantScope:    DDLScopeDatabase,
			wantVerb:     "DROP",
			wantName:     "Order_DB",
			wantIsDropDB: true,
		},
		{
			name:         "mysql_drop_schema_is_database",
			dialect:      DialectMySQL,
			sql:          "DROP SCHEMA IF EXISTS shop_v2;",
			wantScope:    DDLScopeDatabase,
			wantVerb:     "DROP",
			wantName:     "shop_v2",
			wantIsDropDB: true,
		},
		{
			name:      "mysql_create_table_qualified_db",
			dialect:   DialectMySQL,
			sql:       "CREATE TABLE `billing`.`invoices` (id INT PRIMARY KEY);",
			wantScope: DDLScopeObject,
			wantVerb:  "CREATE",
			wantDB:    "billing",
			wantName:  "invoices",
		},
		{
			name:         "mysql_rename_table_cross_db_and_multi_pair",
			dialect:      DialectMySQL,
			sql:          "RENAME TABLE db1.old_users TO db2.users, `db3`.`t_a` TO `db4`.`t_b`;",
			wantScope:    DDLScopeObject,
			wantVerb:     "RENAME",
			wantDB:       "db1",
			wantName:     "old_users",
			wantExtraDBs: []string{"db2", "db3", "db4"},
		},
		{
			name:      "mysql_definer_view",
			dialect:   DialectMySQL,
			sql:       "CREATE ALGORITHM=UNDEFINED DEFINER=`root`@`localhost` SQL SECURITY DEFINER VIEW `sales`.`v_top` AS SELECT 1;",
			wantScope: DDLScopeObject,
			wantVerb:  "CREATE",
			wantDB:    "sales",
			wantName:  "v_top",
		},
		{
			name:      "sqlite_create_view_bracket_ident",
			dialect:   DialectSQLite,
			sql:       "CREATE VIEW IF NOT EXISTS [active users] AS SELECT 1;",
			wantScope: DDLScopeObject,
			wantVerb:  "CREATE",
			wantName:  "active users",
		},
		{
			name:      "non_ddl_select_ignored",
			dialect:   DialectPostgres,
			sql:       "SELECT * FROM pg_database;",
			wantScope: DDLScopeNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := InspectDDL(tc.sql, tc.dialect)
			if got.Scope != tc.wantScope {
				t.Fatalf("Scope = %q, want %q (got=%+v)", got.Scope, tc.wantScope, got)
			}
			if got.Verb != tc.wantVerb {
				t.Fatalf("Verb = %q, want %q", got.Verb, tc.wantVerb)
			}
			if got.TargetName != tc.wantName {
				t.Fatalf("TargetName = %q, want %q", got.TargetName, tc.wantName)
			}
			if got.TargetDB != tc.wantDB {
				t.Fatalf("TargetDB = %q, want %q", got.TargetDB, tc.wantDB)
			}
			if got.TargetSchema != tc.wantSchema {
				t.Fatalf("TargetSchema = %q, want %q", got.TargetSchema, tc.wantSchema)
			}
			if got.IsDropDB != tc.wantIsDropDB {
				t.Fatalf("IsDropDB = %v, want %v", got.IsDropDB, tc.wantIsDropDB)
			}
			if got.HasForce != tc.wantHasForce {
				t.Fatalf("HasForce = %v, want %v", got.HasForce, tc.wantHasForce)
			}
			if len(tc.wantExtraDBs) > 0 {
				if len(got.ExtraObjects) != len(tc.wantExtraDBs) {
					t.Fatalf("ExtraObjects = %+v, want DBs %v", got.ExtraObjects, tc.wantExtraDBs)
				}
				for i, wantDB := range tc.wantExtraDBs {
					if got.ExtraObjects[i].DB != wantDB {
						t.Fatalf("ExtraObjects[%d].DB = %q, want %q", i, got.ExtraObjects[i].DB, wantDB)
					}
				}
			}
		})
	}
}

func TestClassifyRollbackVsSavepoint(t *testing.T) {
	tests := []struct {
		sql     string
		dialect Dialect
		want    StatementKind
	}{
		{"ROLLBACK;", DialectPostgres, KindRollback},
		{"ROLLBACK WORK;", DialectPostgres, KindRollback},
		{"ROLLBACK TRANSACTION;", DialectPostgres, KindRollback},
		{"ROLLBACK TO sp1;", DialectPostgres, KindRollbackToSavepoint},
		{"ROLLBACK TO SAVEPOINT sp1;", DialectPostgres, KindRollbackToSavepoint},
		{"ROLLBACK WORK TO sp1;", DialectPostgres, KindRollbackToSavepoint},
		{"ROLLBACK TRANSACTION TO SAVEPOINT sp1;", DialectPostgres, KindRollbackToSavepoint},
		{"/* comment */ ROLLBACK TO SAVEPOINT s1;", DialectMySQL, KindRollbackToSavepoint},
		{"ROLLBACK TRANSACTION TO s1;", DialectSQLite, KindRollbackToSavepoint},
		{"ROLLBACK PREPARED 'gid_1';", DialectPostgres, KindOther},
	}

	for _, tc := range tests {
		got, _, _ := Classify(tc.sql, tc.dialect)
		if got != tc.want {
			t.Errorf("Classify(%q, %s) = %q, want %q", tc.sql, tc.dialect, got, tc.want)
		}
	}
}

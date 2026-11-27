package service

import (
	"context"
	"errors"
	"strings"

	"hesper/internal/driver"
	"hesper/internal/dto"
	"hesper/internal/session"
)

// SchemaService uses Track A (independent metadata pool) to browse databases, schemas, tables, views,
// optional sequences, DDL, and completion catalogs without interfering with query tab sessions.
type SchemaService struct {
	sessMgr *session.Manager
}

func NewSchemaService(sessMgr *session.Manager) *SchemaService {
	return &SchemaService{sessMgr: sessMgr}
}

func (s *SchemaService) ListDatabases(ctx context.Context, connID string) ([]dto.DatabaseInfo, error) {
	var out []dto.DatabaseInfo
	runList := func(db string) error {
		return s.sessMgr.WithMetaConn(ctx, connID, db, func(conn driver.Conn) error {
			insp, ok := conn.(driver.MetaInspector)
			if !ok {
				return dto.NewAppError(dto.ErrUnsupported, "当前驱动不支持数据库列表浏览", "")
			}
			var err error
			out, err = insp.ListDatabases(ctx)
			return err
		})
	}
	err := runList("")
	if err != nil {
		var ae *dto.AppError
		if errors.As(err, &ae) {
			if ae.SQLState == "3D000" {
				// Configured defaultDatabase was dropped (PostgreSQL invalid_catalog_name): fall back to maintenance DB.
				err = runList("postgres")
			} else if isMySQLUnknownDatabaseErr(ae) {
				// Configured defaultDatabase was dropped (MySQL/MariaDB 1049): fall back to built-in information_schema.
				err = runList("information_schema")
			}
		}
	}
	return out, wrapErr(err, dto.ErrInternal, "获取数据库列表失败")
}

func isMySQLUnknownDatabaseErr(ae *dto.AppError) bool {
	if ae == nil || ae.SQLState != "42000" {
		return false
	}
	return strings.Contains(ae.Message, "(1049)") ||
		strings.Contains(strings.ToLower(ae.Detail), "unknown database") ||
		strings.Contains(strings.ToLower(ae.Message), "unknown database")
}

func (s *SchemaService) ListSchemas(ctx context.Context, connID, db string) ([]string, error) {
	var out []string
	err := s.sessMgr.WithMetaConn(ctx, connID, db, func(conn driver.Conn) error {
		insp, ok := conn.(driver.MetaInspector)
		if !ok {
			return nil
		}
		var err error
		out, err = insp.ListSchemas(ctx, db)
		return err
	})
	return out, wrapErr(err, dto.ErrInternal, "获取 Schema 列表失败")
}

func (s *SchemaService) ListTables(ctx context.Context, connID, db, schema string) ([]dto.TableSummary, error) {
	var out []dto.TableSummary
	err := s.sessMgr.WithMetaConn(ctx, connID, db, func(conn driver.Conn) error {
		insp, ok := conn.(driver.MetaInspector)
		if !ok {
			return dto.NewAppError(dto.ErrUnsupported, "当前驱动不支持表列表浏览", "")
		}
		var err error
		out, err = insp.ListTables(ctx, db, schema)
		return err
	})
	return out, wrapErr(err, dto.ErrInternal, "获取数据表列表失败")
}

func (s *SchemaService) ListViews(ctx context.Context, connID, db, schema string) ([]dto.ObjectSummary, error) {
	var out []dto.ObjectSummary
	err := s.sessMgr.WithMetaConn(ctx, connID, db, func(conn driver.Conn) error {
		insp, ok := conn.(driver.MetaInspector)
		if !ok {
			return nil
		}
		var err error
		out, err = insp.ListViews(ctx, db, schema)
		return err
	})
	return out, wrapErr(err, dto.ErrInternal, "获取视图列表失败")
}

// ListSequences uses optional driver.SequenceInspector so non-sequence engines (SQLite, MySQL) are unaffected.
func (s *SchemaService) ListSequences(ctx context.Context, connID, db, schema string) ([]dto.ObjectSummary, error) {
	var out []dto.ObjectSummary
	err := s.sessMgr.WithMetaConn(ctx, connID, db, func(conn driver.Conn) error {
		seqInsp, ok := conn.(driver.SequenceInspector)
		if !ok {
			return nil
		}
		var err error
		out, err = seqInsp.ListSequences(ctx, db, schema)
		return err
	})
	return out, wrapErr(err, dto.ErrInternal, "获取序列列表失败")
}

func (s *SchemaService) GetTableDetail(ctx context.Context, connID, db, schema, table string) (*dto.TableDetail, error) {
	var out *dto.TableDetail
	err := s.sessMgr.WithMetaConn(ctx, connID, db, func(conn driver.Conn) error {
		insp, ok := conn.(driver.MetaInspector)
		if !ok {
			return dto.NewAppError(dto.ErrUnsupported, "当前驱动不支持表结构浏览", "")
		}
		var err error
		out, err = insp.GetTableDetail(ctx, db, schema, table)
		return err
	})
	return out, wrapErr(err, dto.ErrInternal, "获取表结构失败")
}

func (s *SchemaService) GetTableDDL(ctx context.Context, connID, db, schema, table string) (string, error) {
	var out string
	err := s.sessMgr.WithMetaConn(ctx, connID, db, func(conn driver.Conn) error {
		insp, ok := conn.(driver.MetaInspector)
		if !ok {
			return dto.NewAppError(dto.ErrUnsupported, "当前驱动不支持 DDL 查看", "")
		}
		var err error
		out, err = insp.GetTableDDL(ctx, db, schema, table)
		return err
	})
	return out, wrapErr(err, dto.ErrInternal, "获取表 DDL 失败")
}

func (s *SchemaService) FetchCompletionCatalog(ctx context.Context, connID, db, schema string) (*dto.CompletionCatalog, error) {
	var out *dto.CompletionCatalog
	err := s.sessMgr.WithMetaConn(ctx, connID, db, func(conn driver.Conn) error {
		insp, ok := conn.(driver.MetaInspector)
		if !ok {
			return nil
		}
		var err error
		out, err = insp.FetchCompletionCatalog(ctx, db, schema)
		return err
	})
	return out, wrapErr(err, dto.ErrInternal, "加载补全字典失败")
}

import { EngineType, RowIdentityKind, type TableEditability } from '@/api/types'

/**
 * Quotes a single SQL identifier according to the active engine dialect.
 * PostgreSQL and SQLite use double quotes; MySQL and MariaDB use backticks.
 */
export function quoteIdent(ident: string, dialect?: EngineType): string {
  if (dialect === EngineType.EnginePostgres || dialect === EngineType.EngineSQLite) {
    return `"${ident.replace(/"/g, '""')}"`
  }
  return `\`${ident.replace(/`/g, '``')}\``
}

export interface TableTarget {
  schema?: string
  table: string
}

/**
 * Quotes a structured { schema, table } reference segment-by-segment without splitting on '.'
 * or trimming whitespace, preserving exact identifier boundaries (e.g. "order.items" or " orders ").
 */
export function quoteTableTarget(target: TableTarget | null | undefined, dialect?: EngineType): string {
  const table = target?.table
  if (!table) return quoteIdent('table_name', dialect)
  const schema = target?.schema
  return schema ? `${quoteIdent(schema, dialect)}.${quoteIdent(table, dialect)}` : quoteIdent(table, dialect)
}

/**
 * Formats a table's display path (e.g. db.schema.table or table for single-file SQLite).
 */
export function formatTablePath(
  database: string,
  schema: string,
  table: string,
  hasDatabases = true,
): string {
  const parts: string[] = []
  if (hasDatabases && database) parts.push(database)
  if (schema) parts.push(schema)
  parts.push(table)
  return parts.join('.')
}

/**
 * Formats the backend-computed TableEditability into a concise human-readable summary.
 */
export function formatEditabilitySummary(
  editability: TableEditability | null | undefined,
  readOnly = false,
): string {
  if (!editability) return ''
  const parts: string[] = []
  if (editability.storageEngine) parts.push(editability.storageEngine)
  if (editability.rowIdentity === RowIdentityKind.RowIdentityPrimaryKey) {
    parts.push(`主键(${(editability.keyColumns ?? []).join(', ')})`)
  } else if (editability.rowIdentity === RowIdentityKind.RowIdentityUniqueNotNull) {
    parts.push(`非空唯一键(${(editability.keyColumns ?? []).join(', ')})`)
  } else {
    parts.push('无稳定唯一键')
  }
  if (!editability.batchTransactional) {
    parts.push('非事务引擎')
  }
  if (readOnly) {
    parts.push('只读连接')
  }
  return parts.join(' · ')
}

/**
 * Builds a clean dialect-quoted quick SELECT template for a table.
 */
export function buildQuickSelectSQL(
  schema: string,
  table: string,
  dialect?: EngineType,
  limit = 100,
): string {
  const target = quoteTableTarget({ schema, table }, dialect)
  return `SELECT *\nFROM ${target}\nLIMIT ${limit};\n`
}

/**
 * Returns the default TCP port for a network database engine.
 */
export function defaultPortForEngine(driver?: EngineType | string): number {
  switch (driver) {
    case EngineType.EnginePostgres:
      return 5432
    case EngineType.EngineMySQL:
    case EngineType.EngineMariaDB:
      return 3306
    default:
      return 0
  }
}

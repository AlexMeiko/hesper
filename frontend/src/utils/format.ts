import { ValueKind, type CellValue, type ColumnMeta } from '@/api/client'

export function formatBytes(bytes?: number): string {
  if (bytes == null || bytes < 0) return '0 B'
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(2)} MB`
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms} ms`
  if (ms < 60000) return `${(ms / 1000).toFixed(2)} s`
  const mins = Math.floor(ms / 60000)
  const secs = ((ms % 60000) / 1000).toFixed(1)
  return `${mins}m ${secs}s`
}

export function isNumericKind(kind: ValueKind): boolean {
  return (
    kind === ValueKind.KindInt ||
    kind === ValueKind.KindBigInt ||
    kind === ValueKind.KindDecimal ||
    kind === ValueKind.KindFloat
  )
}

export function isEmptyStringCell(cell: CellValue | null | undefined): boolean {
  if (!cell) return false
  return cell.kind !== ValueKind.KindNull && cell.kind !== ValueKind.KindBinary && (cell.text ?? '') === ''
}

export function cellToPlainText(cell: CellValue | null | undefined): string {
  if (!cell || cell.kind === ValueKind.KindNull) return 'NULL'
  return cell.text ?? ''
}

/**
 * Estimates single-line visual pixel width of a string (distinguishing CJK/wide chars vs ASCII/monospace digits).
 * Caps inspection length so huge TEXT cells stay O(1) fast.
 */
export function estimateTextWidth(text: string, isMono = false, maxChars = 160): number {
  const sample = text.length > maxChars ? text.slice(0, maxChars) : text
  const asciiPx = isMono ? 7.9 : 7.4
  const widePx = isMono ? 13.5 : 13.0
  let px = 0
  for (let i = 0; i < sample.length; i++) {
    const code = sample.charCodeAt(i)
    if (code === 10 || code === 13) {
      px += asciiPx
    } else if (code > 0x2e7f) {
      px += widePx
    } else {
      px += asciiPx
    }
  }
  return Math.ceil(px)
}

/**
 * Computes content-aware column width by sampling visible/first rows alongside header metadata.
 * Gives high-precision BIGINT / DECIMAL generous room (up to 420px default) so numbers do not truncate prematurely.
 */
export function estimateColumnWidth(
  col: ColumnMeta,
  rows: (CellValue[] | null)[],
  colIdx: number,
  opts?: { maxSampleRows?: number; autoFit?: boolean },
): number {
  const autoFit = Boolean(opts?.autoFit)
  const sampleLimit = autoFit ? rows.length : Math.min(rows.length, opts?.maxSampleRows ?? 100)
  const isMono = isNumericKind(col.kind) || col.kind === ValueKind.KindDateTime || col.kind === ValueKind.KindBinary

  const headerNamePx = estimateTextWidth(col.name, false) + (col.isPrimaryKey ? 18 : 0) + 34
  const headerTypePx = estimateTextWidth(col.databaseType || col.kind, true) + 24
  let maxPx = Math.max(headerNamePx, headerTypePx)

  for (let r = 0; r < sampleLimit; r++) {
    const cell = rows[r]?.[colIdx]
    if (!cell || cell.kind === ValueKind.KindNull) {
      maxPx = Math.max(maxPx, 68)
      continue
    }
    let sampleText = cell.text ?? ''
    if (cell.kind === ValueKind.KindBinary) {
      sampleText = `BLOB ${formatBytes(cell.byteLength)} · ${sampleText.slice(0, 24)}`
    }
    const cellPx = estimateTextWidth(sampleText, isMono, autoFit ? 220 : 120) + 26
    if (cellPx > maxPx) {
      maxPx = cellPx
    }
  }

  if (autoFit) {
    return Math.max(72, Math.min(maxPx, 800))
  }

  switch (col.kind) {
    case ValueKind.KindBigInt:
    case ValueKind.KindDecimal:
      return Math.max(120, Math.min(maxPx, 420))
    case ValueKind.KindInt:
    case ValueKind.KindBool:
    case ValueKind.KindFloat:
      return Math.max(88, Math.min(maxPx, 200))
    case ValueKind.KindDateTime:
      return Math.max(160, Math.min(maxPx, 240))
    case ValueKind.KindBinary:
      return Math.max(150, Math.min(maxPx, 280))
    default:
      return Math.max(116, Math.min(maxPx, 320))
  }
}

export interface WhitespaceToken {
  text: string
  kind: 'text' | 'space' | 'tab' | 'newline' | 'special'
  lineBreakAfter?: boolean
}

/**
 * Splits text into safe display tokens for rendering invisible whitespace characters
 * without touching raw values or introducing v-html XSS risk.
 */
export function tokenizeWhitespace(text: string, preserveLineBreaks = false): WhitespaceToken[] {
  const tokens: WhitespaceToken[] = []
  let buf = ''

  const flush = () => {
    if (buf) {
      tokens.push({ text: buf, kind: 'text' })
      buf = ''
    }
  }

  for (let i = 0; i < text.length; i++) {
    const ch = text[i]
    if (ch === ' ') {
      flush()
      tokens.push({ text: '·', kind: 'space' })
    } else if (ch === '\u3000') {
      flush()
      tokens.push({ text: '□', kind: 'space' })
    } else if (ch === '\t') {
      flush()
      tokens.push({ text: '→', kind: 'tab' })
    } else if (ch === '\r' && text[i + 1] === '\n') {
      flush()
      tokens.push({ text: '␍↵', kind: 'newline', lineBreakAfter: preserveLineBreaks })
      i++
    } else if (ch === '\n') {
      flush()
      tokens.push({ text: '↵', kind: 'newline', lineBreakAfter: preserveLineBreaks })
    } else if (ch === '\r') {
      flush()
      tokens.push({ text: '␍', kind: 'newline', lineBreakAfter: preserveLineBreaks })
    } else if (ch === '\u00A0') {
      flush()
      tokens.push({ text: '[NBSP]', kind: 'special' })
    } else if (ch === '\u200B') {
      flush()
      tokens.push({ text: '[ZWSP]', kind: 'special' })
    } else if (ch === '\uFEFF') {
      flush()
      tokens.push({ text: '[BOM]', kind: 'special' })
    } else {
      buf += ch
    }
  }
  flush()
  return tokens
}

export function rowsToTSV(columns: ColumnMeta[], rows: (CellValue[] | null)[]): string {
  const header = columns.map((c) => c.name).join('\t')
  const lines = rows.map((row) =>
    columns
      .map((_, i) => {
        const cell = row?.[i]
        if (!cell || cell.kind === ValueKind.KindNull) return ''
        return (cell.text ?? '').replace(/\t/g, ' ').replace(/\r?\n/g, ' ')
      })
      .join('\t'),
  )
  return [header, ...lines].join('\n')
}

export function rowsToCSV(columns: ColumnMeta[], rows: (CellValue[] | null)[]): string {
  const esc = (v: string) => (/[",\n\r]/.test(v) ? `"${v.replace(/"/g, '""')}"` : v)
  const header = columns.map((c) => esc(c.name)).join(',')
  const lines = rows.map((row) =>
    columns
      .map((_, i) => {
        const cell = row?.[i]
        if (!cell || cell.kind === ValueKind.KindNull) return ''
        return esc(cell.text ?? '')
      })
      .join(','),
  )
  return [header, ...lines].join('\n')
}

export function rowsToJSON(columns: ColumnMeta[], rows: (CellValue[] | null)[]): string {
  const objs = rows.map((row) => {
    const rec: Record<string, string | boolean | null> = {}
    columns.forEach((col, i) => {
      const cell = row?.[i]
      if (!cell || cell.kind === ValueKind.KindNull) {
        rec[col.name] = null
      } else if (cell.kind === ValueKind.KindBool) {
        rec[col.name] = cell.text === 'true' || cell.text === '1'
      } else {
        // Preserve BIGINT / DECIMAL as exact JSON strings to prevent precision loss!
        rec[col.name] = cell.text ?? ''
      }
    })
    return rec
  })
  return JSON.stringify(objs, null, 2)
}

export function rowsToInsertSQL(tableName: string, columns: ColumnMeta[], rows: (CellValue[] | null)[]): string {
  const tbl = tableName || 'table_name'
  const colList = columns.map((c) => `\`${c.name}\``).join(', ')
  return rows
    .map((row) => {
      const vals = columns.map((_, i) => {
        const cell = row?.[i]
        if (!cell || cell.kind === ValueKind.KindNull) return 'NULL'
        if (isNumericKind(cell.kind) && cell.text) return cell.text
        const escaped = (cell.text ?? '').replace(/'/g, "''")
        return `'${escaped}'`
      })
      return `INSERT INTO ${tbl} (${colList}) VALUES (${vals.join(', ')});`
    })
    .join('\n')
}

export function formatHexDump(bytes: Uint8Array, maxBytes = 4096): string {
  const len = Math.min(bytes.length, maxBytes)
  const lines: string[] = []
  for (let offset = 0; offset < len; offset += 16) {
    const slice = bytes.subarray(offset, Math.min(offset + 16, len))
    const addr = offset.toString(16).padStart(8, '0')
    const hex = Array.from(slice)
      .map((b) => b.toString(16).padStart(2, '0'))
      .join(' ')
      .padEnd(16 * 3 - 1, ' ')
    const ascii = Array.from(slice)
      .map((b) => (b >= 0x20 && b <= 0x7e ? String.fromCharCode(b) : '.'))
      .join('')
    lines.push(`${addr}  ${hex}  |${ascii}|`)
  }
  if (bytes.length > maxBytes) {
    lines.push(`... (${bytes.length - maxBytes} 更多字节未展开，可点击下载完整文件)`)
  }
  return lines.join('\n')
}

export function envColorHex(envColor?: string): string {
  switch (envColor) {
    case 'red':
      return 'var(--env-red)'
    case 'amber':
      return 'var(--env-amber)'
    case 'green':
      return 'var(--env-green)'
    default:
      return 'var(--env-default)'
  }
}

export function envColorLabel(envColor?: string): string {
  switch (envColor) {
    case 'red':
      return '生产'
    case 'amber':
      return '预发'
    case 'green':
      return '开发'
    default:
      return '默认'
  }
}

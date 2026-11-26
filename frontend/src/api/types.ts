// Pure DTO types, enums, and stateless URL helpers re-exported without pulling in @wailsio/runtime.
// Components and pure utilities should import types/enums from here so unit tests
// and UI modules never initialize the Wails runtime or attempt localhost connections.
export * from '@bindings/sql-manager/internal/dto/models'

export function streamUrl(ref: string, download = false): string {
  return `/stream/${encodeURIComponent(ref)}${download ? '?download=1' : ''}`
}

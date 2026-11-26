import { onBeforeUnmount, ref } from 'vue'

export interface DragResizeOptions {
  axis: 'x' | 'y'
  onMove: (delta: number, ev: MouseEvent) => void
  onEnd?: () => void
}

/**
 * Unified mouse drag-resize helper used by the sidebar splitter,
 * query editor/result vertical splitter, and data grid column resizers.
 */
export function useDragResize() {
  const dragging = ref(false)
  let cleanupActive: (() => void) | null = null

  function stopDrag() {
    if (cleanupActive) {
      cleanupActive()
      cleanupActive = null
    }
  }

  function startDrag(ev: MouseEvent, opts: DragResizeOptions) {
    ev.preventDefault()
    stopDrag()

    const startCoord = opts.axis === 'x' ? ev.clientX : ev.clientY
    const prevCursor = document.body.style.cursor
    const prevUserSelect = document.body.style.userSelect

    dragging.value = true
    document.body.style.cursor = opts.axis === 'x' ? 'col-resize' : 'row-resize'
    document.body.style.userSelect = 'none'

    const handleMove = (moveEv: MouseEvent) => {
      const current = opts.axis === 'x' ? moveEv.clientX : moveEv.clientY
      opts.onMove(current - startCoord, moveEv)
    }

    const handleUp = () => {
      stopDrag()
      opts.onEnd?.()
    }

    cleanupActive = () => {
      dragging.value = false
      document.body.style.cursor = prevCursor
      document.body.style.userSelect = prevUserSelect
      window.removeEventListener('mousemove', handleMove)
      window.removeEventListener('mouseup', handleUp)
    }

    window.addEventListener('mousemove', handleMove)
    window.addEventListener('mouseup', handleUp)
  }

  onBeforeUnmount(stopDrag)

  return {
    dragging,
    startDrag,
    stopDrag,
  }
}

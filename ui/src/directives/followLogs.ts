import type { Directive } from 'vue'

// Hysteresis: distance-from-bottom (px) at which the view is considered "pinned".
// PIN_THRESHOLD re-pins when the user scrolls back near the bottom; UNPIN_THRESHOLD
// is the larger value above which a scroll-up is treated as "user left the bottom".
// Unpin must be > pin so a tiny jitter near the bottom does not flap the state.
export const FOLLOW_PIN_THRESHOLD = 4 // re-pin when within 4px of bottom
export const FOLLOW_UNPIN_THRESHOLD = 24 // unpin once user is >24px from bottom

export interface FollowLogsState {
  pinned: boolean
  programmatic: boolean // true while a directive-driven scroll is in flight
  raf: number | null
  ro: ResizeObserver | null
}

function readState(el: HTMLElement): FollowLogsState {
  return el.__followLogs ?? { pinned: true, programmatic: false, raf: null, ro: null }
}

function distanceFromBottom(el: HTMLElement): number {
  return el.scrollHeight - el.scrollTop - el.clientHeight
}

function scrollToBottom(el: HTMLElement): void {
  readState(el).programmatic = true
  el.scrollTop = el.scrollHeight
  // Clear the guard on the next frame, after the async scroll event has fired.
  requestAnimationFrame(() => {
    readState(el).programmatic = false
  })
}

function onScroll(e: Event): void {
  const el = e.currentTarget as HTMLElement
  const s = readState(el)
  if (s.programmatic) return // ignore scrolls we triggered ourselves
  const d = distanceFromBottom(el)
  if (s.pinned && d > FOLLOW_UNPIN_THRESHOLD) s.pinned = false
  else if (!s.pinned && d <= FOLLOW_PIN_THRESHOLD) s.pinned = true
}

export const vFollowLogs: Directive<HTMLElement> = {
  mounted(el: HTMLElement) {
    const s: FollowLogsState = { pinned: true, programmatic: false, raf: null, ro: null }
    el.__followLogs = s
    el.addEventListener('scroll', onScroll, { passive: true })

    // Re-assert bottom on container resize (window resize, font load, flex change).
    if (typeof ResizeObserver !== 'undefined') {
      s.ro = new ResizeObserver(() => {
        if (readState(el).pinned) scrollToBottom(el)
      })
      s.ro.observe(el)
    }

    // Scroll to end now, then re-assert on the next frame in case the layout was
    // not final at mount (late v-for content / font swap).
    scrollToBottom(el)
    s.raf = requestAnimationFrame(() => {
      if (readState(el).pinned) scrollToBottom(el)
    })
  },
  updated(el: HTMLElement) {
    if (readState(el).pinned) scrollToBottom(el)
  },
  unmounted(el: HTMLElement) {
    const s = readState(el)
    el.removeEventListener('scroll', onScroll)
    if (s.raf !== null) cancelAnimationFrame(s.raf)
    s.ro?.disconnect()
    el.__followLogs = undefined
  },
}

// Augment HTMLElement for the per-element state field (module-private).
declare global {
  interface HTMLElement {
    __followLogs?: FollowLogsState
  }
}

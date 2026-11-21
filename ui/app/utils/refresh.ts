// refreshAfterWrite re-runs a list fetch after a mutation so a read that landed
// on a Raft follower (reads are not leader-pinned, ~1–3 s staleness) is
// corrected automatically instead of leaving the UI on the pre-write state
// until a manual reload.
//
// It is fire-and-forget by design: the fetch runs immediately, then with a
// growing delay (delayMs, 2×delayMs, …, capped at 1500 ms) up to `attempts`
// times. Every attempt keeps the last result, intermediate failures are
// ignored, and only the final failure is logged. Callers must not await it and
// must not show a spinner for these background refreshes.
export function refreshAfterWrite(
  fn: () => Promise<unknown>,
  attempts = 6,
  delayMs = 300,
): Promise<void> {
  const run = async () => {
    for (let attempt = 0; attempt < attempts; attempt++) {
      if (attempt > 0) await delay(Math.min(delayMs * 2 ** (attempt - 1), 1500))
      try {
        await fn()
      } catch (err) {
        if (attempt === attempts - 1) {
          console.error('refreshAfterWrite: background refresh failed', err)
        }
      }
    }
  }
  return run()
}

function delay(ms: number): Promise<void> {
  return new Promise(resolve => window.setTimeout(resolve, ms))
}

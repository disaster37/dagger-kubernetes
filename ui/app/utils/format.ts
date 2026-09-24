export function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '-'
  if (n === 0) return '0 B'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${i === 0 ? v.toFixed(0) : v.toFixed(2)} ${units[i]}`
}

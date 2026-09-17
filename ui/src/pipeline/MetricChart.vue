<template>
  <div class="metric-chart">
    <div class="metric-head">
      <span class="metric-label">{{ series.label }}</span>
      <span class="metric-value">{{ lastValue }}</span>
    </div>
    <div v-if="series.points.length < 2" class="metric-empty">no data</div>
    <svg
      v-else
      class="metric-svg"
      :viewBox="`0 0 ${WIDTH} ${HEIGHT}`"
      preserveAspectRatio="none"
      role="img"
      :aria-label="`${series.label} over time`"
    >
      <polygon class="metric-area" :points="areaPoints" />
      <polyline class="metric-line" :points="linePoints" />
    </svg>
    <div class="metric-axis">
      <span>{{ formatValue(min) }}</span>
      <span>{{ formatValue(max) }}</span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import type { MetricSeries } from '@/api/types'

const props = defineProps<{ series: MetricSeries; unit: string }>()

const WIDTH = 300
const HEIGHT = 60
const PAD = 4

// Reduce rather than Math.min(...points): a 24h window at a 1s step can hold
// 86 400 points, which exceeds the JS engine's spread-argument limit and would
// throw a RangeError.
const min = computed(() => {
  let m = Infinity
  for (const p of props.series.points) if (p.v < m) m = p.v
  return m
})
const max = computed(() => {
  let m = -Infinity
  for (const p of props.series.points) if (p.v > m) m = p.v
  return m
})
const range = computed(() => max.value - min.value || 1)

function x(i: number): number {
  const n = props.series.points.length
  return PAD + (i / (n - 1)) * (WIDTH - 2 * PAD)
}

function y(v: number): number {
  return HEIGHT - PAD - ((v - min.value) / range.value) * (HEIGHT - 2 * PAD)
}

const linePoints = computed(() =>
  props.series.points.map((p, i) => `${x(i)},${y(p.v)}`).join(' ')
)

const areaPoints = computed(
  () => `${PAD},${HEIGHT - PAD} ${linePoints.value} ${WIDTH - PAD},${HEIGHT - PAD}`
)

const lastValue = computed(() => {
  const points = props.series.points
  if (points.length === 0) return '-'
  return formatValue(points[points.length - 1].v)
})

function formatValue(v: number): string {
  if (!Number.isFinite(v)) return '-'
  if (props.unit === 'bytes' || props.unit === 'bytes/s') {
    const suffix = props.unit === 'bytes/s' ? '/s' : ''
    const abs = Math.abs(v)
    if (abs >= 1024 ** 3) return `${(v / 1024 ** 3).toFixed(2)} GiB${suffix}`
    if (abs >= 1024 ** 2) return `${(v / 1024 ** 2).toFixed(2)} MiB${suffix}`
    if (abs >= 1024) return `${(v / 1024).toFixed(2)} KiB${suffix}`
    return `${v.toFixed(0)} B${suffix}`
  }
  if (props.unit === 'cores') return v.toFixed(3)
  return v.toFixed(2)
}
</script>

<style scoped>
.metric-chart {
  background: #0d1117;
  border: 1px solid #21262d;
  border-radius: 4px;
  padding: 8px 10px;
}

.metric-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
}

.metric-label {
  font-size: 12px;
  color: #8b949e;
}

.metric-value {
  font-size: 13px;
  font-weight: 600;
  color: #f0f6fc;
  font-family: monospace;
}

.metric-svg {
  display: block;
  width: 100%;
  height: 60px;
  margin-top: 4px;
}

.metric-area {
  fill: rgba(88, 166, 255, 0.15);
  stroke: none;
}

.metric-line {
  fill: none;
  stroke: #58a6ff;
  stroke-width: 1.5;
  vector-effect: non-scaling-stroke;
}

.metric-axis {
  display: flex;
  justify-content: space-between;
  font-size: 10px;
  color: #8b949e;
  font-family: monospace;
}

.metric-empty {
  padding: 18px 0;
  text-align: center;
  font-size: 12px;
  color: #8b949e;
}
</style>

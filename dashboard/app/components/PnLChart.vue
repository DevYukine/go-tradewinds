<script setup lang="ts">
import type { PnLPoint } from '~/types'

const props = defineProps<{
  history: PnLPoint[]
  loading: boolean
}>()

const treasuryData = computed(() =>
  props.history.map(p => ({
    x: new Date(p.created_at).getTime(),
    treasury: p.treasury,
  }))
)

const pnlData = computed(() =>
  props.history.map(p => ({
    x: new Date(p.created_at).getTime(),
    revenue: p.total_rev,
    costs: -p.total_costs,
    net_pnl: p.net_pnl,
  }))
)

const treasuryCategories = {
  treasury: { name: 'Treasury', color: '#14b8a6' },
}

const pnlCategories = {
  revenue: { name: 'Revenue', color: '#22c55e' },
  costs: { name: 'Costs', color: '#f43f5e' },
  net_pnl: { name: 'Net P&L', color: '#f59e0b' },
}

function formatTime(tick: number) {
  const d = new Date(tick)
  return `${d.getHours().toString().padStart(2, '0')}:${d.getMinutes().toString().padStart(2, '0')}`
}

function formatCurrency(tick: number) {
  const abs = Math.abs(tick)
  const sign = tick < 0 ? '-' : ''
  if (abs >= 1_000_000) return `${sign}$${(abs / 1_000_000).toFixed(1)}M`
  if (abs >= 1_000) return `${sign}$${(abs / 1_000).toFixed(1)}K`
  return `${sign}$${abs}`
}
</script>

<template>
  <div class="space-y-4">
    <!-- Treasury Chart -->
    <div class="bg-slate-800 rounded-lg shadow-lg border border-slate-700 p-5">
      <div class="flex items-center justify-between mb-4">
        <h3 class="text-sm font-semibold text-slate-300 flex items-center gap-2">
          <Icon name="lucide:landmark" class="text-teal-400" />
          Treasury
        </h3>
        <span class="text-xs text-slate-500">{{ history.length }} points</span>
      </div>

      <div v-if="loading" class="h-[200px] flex items-center justify-center">
        <Icon name="mdi:loading" class="animate-spin text-2xl text-slate-500" />
      </div>
      <div v-else-if="history.length === 0" class="h-[200px] flex items-center justify-center text-slate-600 text-sm">
        No data available
      </div>
      <div v-else class="pnl-chart">
        <AreaChart
          :data="treasuryData"
          :categories="treasuryCategories"
          :height="200"
          :x-formatter="formatTime"
          :y-formatter="formatCurrency"
          :x-num-ticks="Math.min(treasuryData.length, 6)"
          :y-num-ticks="5"
          curve-type="monotoneX"
          :line-width="2"
          :gradient-stops="[
            { offset: '0%', stopOpacity: 0.3 },
            { offset: '100%', stopOpacity: 0.02 },
          ]"
          :crosshair-config="{
            color: '#334155',
            strokeColor: '#64748b',
            strokeWidth: 1,
          }"
          :y-grid-line="true"
          :x-grid-line="false"
          :x-domain-line="true"
          :y-domain-line="false"
          :duration="600"
          legend-position="top-right"
          :x-axis-config="{
            tickTextColor: '#64748b',
            tickTextFontSize: '11px',
          }"
          :y-axis-config="{
            tickTextColor: '#64748b',
            tickTextFontSize: '11px',
          }"
        />
      </div>
    </div>

    <!-- P&L Breakdown Chart -->
    <div class="bg-slate-800 rounded-lg shadow-lg border border-slate-700 p-5">
      <div class="flex items-center justify-between mb-4">
        <h3 class="text-sm font-semibold text-slate-300 flex items-center gap-2">
          <Icon name="lucide:line-chart" class="text-emerald-400" />
          P&L Breakdown
        </h3>
      </div>

      <div v-if="loading" class="h-[250px] flex items-center justify-center">
        <Icon name="mdi:loading" class="animate-spin text-2xl text-slate-500" />
      </div>
      <div v-else-if="history.length === 0" class="h-[250px] flex items-center justify-center text-slate-600 text-sm">
        No data available
      </div>
      <div v-else class="pnl-chart">
        <AreaChart
          :data="pnlData"
          :categories="pnlCategories"
          :height="250"
          :x-formatter="formatTime"
          :y-formatter="formatCurrency"
          :x-num-ticks="Math.min(pnlData.length, 6)"
          :y-num-ticks="6"
          curve-type="monotoneX"
          :line-width="2"
          :gradient-stops="[
            { offset: '0%', stopOpacity: 0.3 },
            { offset: '100%', stopOpacity: 0.02 },
          ]"
          :crosshair-config="{
            color: '#334155',
            strokeColor: '#64748b',
            strokeWidth: 1,
          }"
          :y-grid-line="true"
          :x-grid-line="false"
          :x-domain-line="true"
          :y-domain-line="false"
          :duration="600"
          legend-position="top-right"
          :x-axis-config="{
            tickTextColor: '#64748b',
            tickTextFontSize: '11px',
          }"
          :y-axis-config="{
            tickTextColor: '#64748b',
            tickTextFontSize: '11px',
          }"
        />
      </div>
    </div>
  </div>
</template>

<style scoped>
.pnl-chart :deep(svg) {
  overflow: visible;
}
.pnl-chart :deep(.unovis-axis-grid line) {
  stroke: #1e293b;
}
.pnl-chart :deep(.unovis-axis line),
.pnl-chart :deep(.unovis-axis path) {
  stroke: #334155;
}
</style>

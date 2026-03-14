<script setup lang="ts">
import type { PnLPoint } from '~/types'

const props = defineProps<{
  companyId: number
}>()

const { history, loading, fetchHistory, connectSSE, disconnectSSE } = usePnL()

watch(
  () => props.companyId,
  async (id) => {
    if (id) {
      await fetchHistory(id)
      connectSSE(id)
    }
  },
  { immediate: true }
)

onUnmounted(() => {
  disconnectSSE()
})

const chartData = computed(() => ({
  labels: history.value.map((p) => {
    const d = new Date(p.created_at)
    return `${d.getHours().toString().padStart(2, '0')}:${d.getMinutes().toString().padStart(2, '0')}`
  }),
  datasets: [
    {
      label: 'Treasury',
      data: history.value.map((p) => p.treasury),
      borderColor: '#10b981',
      backgroundColor: 'rgba(16, 185, 129, 0.1)',
      fill: true,
      tension: 0.3,
      pointRadius: 0,
      borderWidth: 2,
    },
    {
      label: 'Net P&L',
      data: history.value.map((p) => p.net_pnl),
      borderColor: '#3b82f6',
      backgroundColor: 'transparent',
      fill: false,
      tension: 0.3,
      pointRadius: 0,
      borderWidth: 1.5,
      borderDash: [4, 4],
    },
  ],
}))

const chartOptions = computed(() => ({
  responsive: true,
  maintainAspectRatio: false,
  plugins: {
    legend: {
      labels: {
        color: '#94a3b8',
        usePointStyle: true,
        pointStyleWidth: 8,
      },
    },
    tooltip: {
      backgroundColor: '#1e293b',
      titleColor: '#e2e8f0',
      bodyColor: '#94a3b8',
      borderColor: '#334155',
      borderWidth: 1,
    },
  },
  scales: {
    x: {
      ticks: { color: '#64748b', maxTicksLimit: 12 },
      grid: { color: '#1e293b' },
    },
    y: {
      ticks: { color: '#64748b' },
      grid: { color: '#1e293b' },
    },
  },
}))
</script>

<template>
  <div class="bg-slate-800 rounded-lg shadow-lg border border-slate-700 p-5">
    <div class="flex items-center justify-between mb-4">
      <h3 class="text-sm font-semibold text-slate-300 flex items-center gap-2">
        <Icon name="lucide:line-chart" class="text-emerald-400" />
        P&L History
      </h3>
      <span class="text-xs text-slate-500">{{ history.length }} data points</span>
    </div>

    <div v-if="loading" class="h-64 flex items-center justify-center">
      <Icon name="mdi:loading" class="animate-spin text-2xl text-slate-500" />
    </div>

    <div v-else-if="history.length === 0" class="h-64 flex items-center justify-center text-slate-600 text-sm">
      No P&L data available
    </div>

    <div v-else class="h-64">
      <Chart type="line" :data="chartData" :options="chartOptions" />
    </div>
  </div>
</template>

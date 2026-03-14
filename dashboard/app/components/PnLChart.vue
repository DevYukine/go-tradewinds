<script setup lang="ts">
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

const chartData = computed(() =>
  history.value.map((p) => ({
    time: new Date(p.created_at).getTime(),
    treasury: p.treasury,
    net_pnl: p.net_pnl,
  }))
)

const categories = {
  treasury: { name: 'Treasury', color: '#10b981' },
  net_pnl: { name: 'Net P&L', color: '#3b82f6' },
}

function formatTime(tick: number) {
  const d = new Date(tick)
  return `${d.getHours().toString().padStart(2, '0')}:${d.getMinutes().toString().padStart(2, '0')}`
}

function formatValue(tick: number) {
  if (tick >= 1_000_000) return `${(tick / 1_000_000).toFixed(1)}M`
  if (tick >= 1_000) return `${(tick / 1_000).toFixed(1)}K`
  if (tick <= -1_000_000) return `${(tick / 1_000_000).toFixed(1)}M`
  if (tick <= -1_000) return `${(tick / 1_000).toFixed(1)}K`
  return tick.toString()
}
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

    <div v-else>
      <LineChart
        :data="chartData"
        :categories="categories"
        :height="256"
        :x-formatter="formatTime"
        :y-formatter="formatValue"
        :x-num-ticks="8"
        :y-num-ticks="6"
        x-label="Time"
        y-label="Value"
        :y-grid-line="true"
        :x-grid-line="false"
        :line-width="2"
        curve-type="monotoneX"
      />
    </div>
  </div>
</template>

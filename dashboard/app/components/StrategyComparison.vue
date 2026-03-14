<script setup lang="ts">
const { metrics, loading, fetchMetrics } = useStrategyMetrics()

const chartData = computed(() => ({
  labels: metrics.value.map((m) => m.strategy_name),
  datasets: [
    {
      label: 'Avg Profit/Trade',
      data: metrics.value.map((m) => m.avg_profit_per_trade),
      backgroundColor: metrics.value.map((m) =>
        m.avg_profit_per_trade >= 0 ? 'rgba(16, 185, 129, 0.6)' : 'rgba(244, 63, 94, 0.6)'
      ),
      borderColor: metrics.value.map((m) =>
        m.avg_profit_per_trade >= 0 ? '#10b981' : '#f43f5e'
      ),
      borderWidth: 1,
    },
  ],
}))

const chartOptions = computed(() => ({
  responsive: true,
  maintainAspectRatio: false,
  plugins: {
    legend: { display: false },
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
      ticks: { color: '#64748b' },
      grid: { color: '#1e293b' },
    },
    y: {
      ticks: { color: '#64748b' },
      grid: { color: '#1e293b' },
    },
  },
}))

function formatNumber(n: number): string {
  return new Intl.NumberFormat('en-US', { maximumFractionDigits: 1 }).format(n)
}

function winRateColor(rate: number): string {
  if (rate >= 0.6) return 'text-emerald-400'
  if (rate >= 0.4) return 'text-yellow-400'
  return 'text-rose-400'
}
</script>

<template>
  <div class="bg-slate-800 rounded-lg shadow-lg border border-slate-700 p-5">
    <div class="flex items-center justify-between mb-4">
      <h3 class="text-sm font-semibold text-slate-300 flex items-center gap-2">
        <Icon name="lucide:bar-chart-3" class="text-blue-400" />
        Strategy Comparison
      </h3>
      <button
        class="text-xs text-slate-500 hover:text-slate-300 transition-colors"
        @click="fetchMetrics"
      >
        <Icon name="lucide:refresh-cw" class="mr-1" />
        Refresh
      </button>
    </div>

    <div v-if="loading" class="flex items-center justify-center py-8">
      <Icon name="mdi:loading" class="animate-spin text-2xl text-slate-500" />
    </div>

    <template v-else-if="metrics.length > 0">
      <div class="h-48 mb-4">
        <Chart type="bar" :data="chartData" :options="chartOptions" />
      </div>

      <div class="overflow-x-auto">
        <table class="w-full text-xs">
          <thead>
            <tr class="text-slate-500 border-b border-slate-700">
              <th class="text-left py-2 px-2">Strategy</th>
              <th class="text-right py-2 px-2">Companies</th>
              <th class="text-right py-2 px-2">Trades</th>
              <th class="text-right py-2 px-2">Mean</th>
              <th class="text-right py-2 px-2">Std Dev</th>
              <th class="text-right py-2 px-2">CI Low</th>
              <th class="text-right py-2 px-2">CI High</th>
              <th class="text-right py-2 px-2">Win Rate</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="m in metrics"
              :key="m.strategy_name"
              class="border-b border-slate-700/50 text-slate-300"
            >
              <td class="py-2 px-2 font-medium">{{ m.strategy_name }}</td>
              <td class="text-right py-2 px-2 font-mono">{{ m.company_count }}</td>
              <td class="text-right py-2 px-2 font-mono">{{ m.trades_executed }}</td>
              <td
                class="text-right py-2 px-2 font-mono"
                :class="m.avg_profit_per_trade >= 0 ? 'text-emerald-400' : 'text-rose-400'"
              >
                {{ formatNumber(m.avg_profit_per_trade) }}
              </td>
              <td class="text-right py-2 px-2 font-mono text-slate-500">{{ formatNumber(m.std_dev_profit) }}</td>
              <td class="text-right py-2 px-2 font-mono text-slate-500">{{ formatNumber(m.confidence_low) }}</td>
              <td class="text-right py-2 px-2 font-mono text-slate-500">{{ formatNumber(m.confidence_high) }}</td>
              <td class="text-right py-2 px-2 font-mono" :class="winRateColor(m.win_rate)">
                {{ (m.win_rate * 100).toFixed(1) }}%
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>

    <div v-else class="text-center text-slate-600 text-sm py-8">
      No strategy metrics available
    </div>
  </div>
</template>

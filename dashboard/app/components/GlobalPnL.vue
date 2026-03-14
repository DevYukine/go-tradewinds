<script setup lang="ts">
const { data, loading, fetchGlobalPnL } = useGlobalPnL()
const { now } = useNow()
const lastFetch = ref(0)

const lastUpdatedAgo = computed(() => {
  if (!lastFetch.value) return ''
  const secs = Math.floor((now.value - lastFetch.value) / 1000)
  if (secs < 5) return 'just now'
  if (secs < 60) return `${secs}s ago`
  return `${Math.floor(secs / 60)}m ago`
})

let pollTimer: ReturnType<typeof setInterval> | null = null

async function refresh() {
  await fetchGlobalPnL()
  lastFetch.value = Date.now()
}

onMounted(() => {
  refresh()
  pollTimer = setInterval(refresh, 15000)
})

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})

function formatCurrency(value: number): string {
  return new Intl.NumberFormat('en-US').format(value)
}

function pct(value: number): string {
  return (value * 100).toFixed(1) + '%'
}

const STRATEGY_COLORS: Record<string, string> = {
  arbitrage: 'text-emerald-400',
  bulk_hauler: 'text-blue-400',
  market_maker: 'text-amber-400',
}
</script>

<template>
  <div class="bg-slate-800 rounded-lg shadow-lg border border-slate-700 p-5">
    <div class="flex items-center justify-between mb-4">
      <h3 class="text-sm font-semibold text-slate-300 flex items-center gap-2">
        <Icon name="lucide:bar-chart-3" class="text-emerald-400" />
        Global Win / Loss
      </h3>
      <div class="flex items-center gap-3">
        <span v-if="lastUpdatedAgo" class="text-[10px] text-slate-600">Updated {{ lastUpdatedAgo }}</span>
        <button class="text-xs text-slate-500 hover:text-slate-300 transition-colors" @click="refresh">
          <Icon name="lucide:refresh-cw" class="mr-1" />
          Refresh
        </button>
      </div>
    </div>

    <div v-if="loading && !data" class="flex items-center justify-center py-8">
      <Icon name="mdi:loading" class="animate-spin text-2xl text-slate-500" />
    </div>

    <template v-else-if="data">
      <!-- Global Totals -->
      <div class="grid grid-cols-2 sm:grid-cols-4 gap-3 mb-4">
        <div class="bg-slate-900/50 rounded-lg p-3">
          <div class="text-[10px] text-slate-500 uppercase">Net P&amp;L</div>
          <div
            class="text-lg font-bold font-mono"
            :class="data.totals.net_pnl >= 0 ? 'text-emerald-400' : 'text-rose-400'"
          >
            {{ data.totals.net_pnl >= 0 ? '+' : '' }}{{ formatCurrency(data.totals.net_pnl) }}
          </div>
        </div>
        <div class="bg-slate-900/50 rounded-lg p-3">
          <div class="text-[10px] text-slate-500 uppercase">Win Rate</div>
          <div class="text-lg font-bold font-mono text-slate-200">
            {{ pct(data.totals.win_rate) }}
          </div>
          <div class="text-[10px] text-slate-500">
            {{ data.totals.win_count }} / {{ data.totals.trade_count }} trades
          </div>
        </div>
        <div class="bg-slate-900/50 rounded-lg p-3">
          <div class="text-[10px] text-slate-500 uppercase">Trade Revenue</div>
          <div class="text-sm font-bold font-mono text-emerald-400">{{ formatCurrency(data.totals.trade_rev) }}</div>
          <div class="text-[10px] text-slate-500 font-mono">-{{ formatCurrency(data.totals.trade_costs) }} costs</div>
        </div>
        <div class="bg-slate-900/50 rounded-lg p-3">
          <div class="text-[10px] text-slate-500 uppercase">Passenger Rev</div>
          <div class="text-sm font-bold font-mono text-purple-400">+{{ formatCurrency(data.totals.passenger_rev) }}</div>
        </div>
      </div>

      <!-- Per-Company Table -->
      <div class="-mr-3 max-h-[20rem] overflow-y-auto scroll-stable">
        <table class="w-full text-sm pr-3">
          <thead class="sticky top-0 bg-slate-800 z-10">
            <tr class="text-xs text-slate-500 uppercase tracking-wide border-b border-slate-700">
              <th class="text-left py-2 pr-3">Company</th>
              <th class="text-left py-2 pr-3">Strategy</th>
              <th class="text-right py-2 pr-3">Treasury</th>
              <th class="text-right py-2 pr-3">Net P&amp;L</th>
              <th class="text-right py-2 pr-3">Win Rate</th>
              <th class="text-right py-2 pr-3">Trades</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="comp in data.companies"
              :key="comp.company_id"
              class="border-b border-slate-700/30 hover:bg-slate-700/20 transition-colors"
            >
              <td class="py-1.5 pr-3 text-slate-300 text-xs font-medium">{{ comp.company_name }}</td>
              <td class="py-1.5 pr-3">
                <span class="text-xs font-medium" :class="STRATEGY_COLORS[comp.strategy] || 'text-slate-400'">
                  {{ comp.strategy }}
                </span>
              </td>
              <td class="py-1.5 pr-3 text-right text-slate-300 font-mono text-xs">{{ formatCurrency(comp.treasury) }}</td>
              <td
                class="py-1.5 pr-3 text-right font-mono text-xs font-medium"
                :class="comp.net_pnl >= 0 ? 'text-emerald-400' : 'text-rose-400'"
              >
                {{ comp.net_pnl >= 0 ? '+' : '' }}{{ formatCurrency(comp.net_pnl) }}
              </td>
              <td class="py-1.5 pr-3 text-right text-slate-400 font-mono text-xs">{{ pct(comp.win_rate) }}</td>
              <td class="py-1.5 pr-3 text-right text-slate-400 font-mono text-xs">{{ comp.trade_count }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>
  </div>
</template>

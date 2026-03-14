<script setup lang="ts">
const props = defineProps<{
  companyId: number
}>()

const { trades, loading, fetchTrades } = useTrades()

watch(
  () => props.companyId,
  (id) => {
    if (id) fetchTrades(id)
  },
  { immediate: true }
)

function formatCurrency(value: number): string {
  return new Intl.NumberFormat('en-US').format(value)
}

function formatTime(dateStr: string): string {
  const d = new Date(dateStr)
  return d.toLocaleTimeString('en-US', { hour12: false })
}
</script>

<template>
  <div class="bg-slate-800 rounded-lg shadow-lg border border-slate-700 p-5">
    <div class="flex items-center justify-between mb-4">
      <h3 class="text-sm font-semibold text-slate-300 flex items-center gap-2">
        <Icon name="lucide:arrow-left-right" class="text-amber-400" />
        Trade History
      </h3>
      <button
        class="text-xs text-slate-500 hover:text-slate-300 transition-colors"
        @click="fetchTrades(companyId)"
      >
        <Icon name="lucide:refresh-cw" class="mr-1" />
        Refresh
      </button>
    </div>

    <div v-if="loading" class="flex items-center justify-center py-8">
      <Icon name="mdi:loading" class="animate-spin text-2xl text-slate-500" />
    </div>

    <div v-else-if="trades.length === 0" class="text-center text-slate-600 text-sm py-8">
      No trades recorded yet
    </div>

    <div v-else class="overflow-x-auto max-h-80 overflow-y-auto">
      <table class="w-full text-sm">
        <thead class="sticky top-0 bg-slate-800">
          <tr class="text-xs text-slate-500 uppercase tracking-wide border-b border-slate-700">
            <th class="text-left py-2 pr-3">Time</th>
            <th class="text-left py-2 pr-3">Action</th>
            <th class="text-left py-2 pr-3">Good</th>
            <th class="text-left py-2 pr-3">Port</th>
            <th class="text-right py-2 pr-3">Qty</th>
            <th class="text-right py-2 pr-3">Unit Price</th>
            <th class="text-right py-2">Total</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="trade in trades"
            :key="trade.id"
            class="border-b border-slate-700/30 hover:bg-slate-700/20 transition-colors"
          >
            <td class="py-2 pr-3 text-xs text-slate-500 font-mono whitespace-nowrap">
              {{ formatTime(trade.created_at) }}
            </td>
            <td class="py-2 pr-3">
              <span
                class="px-2 py-0.5 rounded-full text-[10px] font-medium uppercase"
                :class="trade.action === 'buy'
                  ? 'bg-sky-500/20 text-sky-400'
                  : 'bg-emerald-500/20 text-emerald-400'"
              >
                {{ trade.action }}
              </span>
            </td>
            <td class="py-2 pr-3 text-slate-300">{{ trade.good_name }}</td>
            <td class="py-2 pr-3 text-slate-400 text-xs">{{ trade.port_name }}</td>
            <td class="py-2 pr-3 text-right text-slate-300 font-mono">{{ trade.quantity }}</td>
            <td class="py-2 pr-3 text-right text-slate-400 font-mono text-xs">{{ formatCurrency(trade.unit_price) }}</td>
            <td
              class="py-2 text-right font-mono font-medium"
              :class="trade.action === 'sell' ? 'text-emerald-400' : 'text-sky-400'"
            >
              {{ formatCurrency(trade.total_price) }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<script setup lang="ts">
import type { PriceEntry } from '~/types'

const { prices, loading, fetchPrices } = usePrices()

onMounted(() => fetchPrices())

let pollTimer: ReturnType<typeof setInterval> | null = null
onMounted(() => {
  pollTimer = setInterval(fetchPrices, 30000)
})
onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})

const selectedPort = ref<string>('')

const portNames = computed(() => {
  const names = new Set(prices.value.map(p => p.port_name))
  return Array.from(names).sort()
})

const filteredPrices = computed(() => {
  if (!selectedPort.value) return prices.value
  return prices.value.filter(p => p.port_name === selectedPort.value)
})

// Group by port for display
const pricesByPort = computed(() => {
  const grouped: Record<string, PriceEntry[]> = {}
  for (const p of filteredPrices.value) {
    if (!grouped[p.port_name]) grouped[p.port_name] = []
    grouped[p.port_name].push(p)
  }
  return grouped
})

function formatCurrency(value: number): string {
  if (value === 0) return '---'
  return new Intl.NumberFormat('en-US').format(value)
}

function spreadColor(spread: number): string {
  if (spread > 0) return 'text-emerald-400'
  if (spread < 0) return 'text-rose-400'
  return 'text-slate-500'
}
</script>

<template>
  <div class="bg-slate-800 rounded-lg shadow-lg border border-slate-700 p-5">
    <div class="flex items-center justify-between mb-4">
      <h3 class="text-sm font-semibold text-slate-300 flex items-center gap-2">
        <Icon name="lucide:bar-chart-3" class="text-emerald-400" />
        Market Prices
      </h3>
      <div class="flex items-center gap-2">
        <select
          v-model="selectedPort"
          class="bg-slate-900 border border-slate-700 rounded text-xs text-slate-300 px-2 py-1"
        >
          <option value="">All Ports</option>
          <option v-for="port in portNames" :key="port" :value="port">{{ port }}</option>
        </select>
        <button
          class="text-xs text-slate-500 hover:text-slate-300 transition-colors"
          @click="fetchPrices"
        >
          <Icon name="lucide:refresh-cw" />
        </button>
      </div>
    </div>

    <div v-if="loading && prices.length === 0" class="flex items-center justify-center py-8">
      <Icon name="mdi:loading" class="animate-spin text-2xl text-slate-500" />
    </div>

    <div v-else-if="prices.length === 0" class="text-center text-slate-600 text-sm py-8">
      No price data available yet
    </div>

    <div v-else class="max-h-96 overflow-y-auto space-y-4 pr-2">
      <div v-for="(portPrices, portName) in pricesByPort" :key="portName">
        <div class="text-xs font-semibold text-slate-400 uppercase tracking-wide mb-1.5 flex items-center gap-1.5">
          <Icon name="lucide:map-pin" class="text-sky-400" />
          {{ portName }}
        </div>
        <div class="overflow-x-auto">
          <table class="w-full text-xs">
            <thead>
              <tr class="text-slate-500 border-b border-slate-700/50">
                <th class="text-left py-1 pr-2">Good</th>
                <th class="text-right py-1 pr-2">Buy</th>
                <th class="text-right py-1 pr-2">Sell</th>
                <th class="text-right py-1">Spread</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="price in portPrices"
                :key="price.good_id"
                class="border-b border-slate-700/20"
              >
                <td class="py-1 pr-2 text-slate-300">{{ price.good_name }}</td>
                <td class="py-1 pr-2 text-right font-mono text-sky-400">{{ formatCurrency(price.buy_price) }}</td>
                <td class="py-1 pr-2 text-right font-mono text-emerald-400">{{ formatCurrency(price.sell_price) }}</td>
                <td class="py-1 text-right font-mono" :class="spreadColor(price.spread)">
                  {{ price.spread > 0 ? '+' : '' }}{{ formatCurrency(price.spread) }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>
  </div>
</template>

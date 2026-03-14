<script setup lang="ts">
const props = defineProps<{
  companyId: number
}>()

const activeTab = ref<'trades' | 'passengers'>('trades')
const { trades, loading, fetchTrades, lastUpdated } = useTrades()
const { passengers, loading: passengersLoading, fetchPassengers, lastUpdated: passengersLastUpdated } = usePassengers()
const { now } = useNow()

const currentLastUpdated = computed(() =>
  activeTab.value === 'trades' ? lastUpdated.value : passengersLastUpdated.value
)

const lastUpdatedAgo = computed(() => {
  if (!currentLastUpdated.value) return ''
  const secs = Math.floor((now.value - currentLastUpdated.value) / 1000)
  if (secs < 5) return 'just now'
  if (secs < 60) return `${secs}s ago`
  return `${Math.floor(secs / 60)}m ago`
})

let pollTimer: ReturnType<typeof setInterval> | null = null

function startPolling(id: number) {
  if (pollTimer) clearInterval(pollTimer)
  fetchTrades(id)
  fetchPassengers(id)
  pollTimer = setInterval(() => {
    fetchTrades(id)
    fetchPassengers(id)
  }, 15000)
}

watch(
  () => props.companyId,
  (id) => {
    if (id) startPolling(id)
  },
  { immediate: true }
)

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})

function refresh() {
  fetchTrades(props.companyId)
  fetchPassengers(props.companyId)
}

function formatCurrency(value: number): string {
  return new Intl.NumberFormat('en-US').format(value)
}

function formatTime(dateStr: string): string {
  const d = new Date(dateStr)
  return d.toLocaleTimeString('en-US', { hour12: false })
}

const totalBought = computed(() =>
  trades.value.filter(t => t.action === 'buy').reduce((s, t) => s + t.total_price, 0)
)

const totalSold = computed(() =>
  trades.value.filter(t => t.action === 'sell').reduce((s, t) => s + t.total_price, 0)
)

const tradeProfit = computed(() => totalSold.value - totalBought.value)

const totalPassengerRev = computed(() =>
  passengers.value.reduce((s, p) => s + p.bid, 0)
)

const totalPassengerCount = computed(() =>
  passengers.value.reduce((s, p) => s + p.count, 0)
)
</script>

<template>
  <div class="bg-slate-800 rounded-lg shadow-lg border border-slate-700 p-5">
    <div class="flex items-center justify-between mb-4">
      <h3 class="text-sm font-semibold text-slate-300 flex items-center gap-2">
        <Icon name="lucide:arrow-left-right" class="text-amber-400" />
        Trade History
      </h3>
      <div class="flex items-center gap-3">
        <span v-if="lastUpdatedAgo" class="text-[10px] text-slate-600">Updated {{ lastUpdatedAgo }}</span>
        <button
          class="text-xs text-slate-500 hover:text-slate-300 transition-colors"
          @click="refresh"
        >
          <Icon name="lucide:refresh-cw" class="mr-1" />
          Refresh
        </button>
      </div>
    </div>

    <!-- Tab Switcher -->
    <div class="flex gap-1 mb-3 border-b border-slate-700 pb-2">
      <button
        class="px-3 py-1 rounded text-xs font-medium transition-colors"
        :class="activeTab === 'trades'
          ? 'bg-slate-700 text-slate-200'
          : 'text-slate-500 hover:text-slate-300'"
        @click="activeTab = 'trades'"
      >
        Trades
        <span class="ml-1 text-slate-500">{{ trades.length }}</span>
      </button>
      <button
        class="px-3 py-1 rounded text-xs font-medium transition-colors"
        :class="activeTab === 'passengers'
          ? 'bg-purple-700/50 text-purple-200'
          : 'text-slate-500 hover:text-slate-300'"
        @click="activeTab = 'passengers'"
      >
        Passengers
        <span class="ml-1 text-slate-500">{{ passengers.length }}</span>
      </button>
    </div>

    <!-- Trades Tab -->
    <template v-if="activeTab === 'trades'">
      <div v-if="loading && trades.length === 0" class="flex items-center justify-center py-8">
        <Icon name="mdi:loading" class="animate-spin text-2xl text-slate-500" />
      </div>

      <div v-else-if="trades.length === 0" class="text-center text-slate-600 text-sm py-8">
        No trades recorded yet
      </div>

      <template v-else>
        <!-- Trade Summary -->
        <div class="grid grid-cols-3 gap-3 mb-3">
          <div class="bg-slate-900/50 rounded-lg p-2">
            <div class="text-[10px] text-slate-500 uppercase">Bought</div>
            <div class="text-sm font-bold text-sky-400 font-mono">{{ formatCurrency(totalBought) }}</div>
          </div>
          <div class="bg-slate-900/50 rounded-lg p-2">
            <div class="text-[10px] text-slate-500 uppercase">Sold</div>
            <div class="text-sm font-bold text-emerald-400 font-mono">{{ formatCurrency(totalSold) }}</div>
          </div>
          <div class="bg-slate-900/50 rounded-lg p-2">
            <div class="text-[10px] text-slate-500 uppercase">Profit</div>
            <div
              class="text-sm font-bold font-mono"
              :class="tradeProfit >= 0 ? 'text-emerald-400' : 'text-rose-400'"
            >
              {{ tradeProfit >= 0 ? '+' : '' }}{{ formatCurrency(tradeProfit) }}
            </div>
          </div>
        </div>

        <div class="max-h-72 overflow-y-auto pr-2">
          <table class="w-full text-sm">
            <thead class="sticky top-0 bg-slate-800 z-10">
              <tr class="text-xs text-slate-500 uppercase tracking-wide border-b border-slate-700">
                <th class="text-left py-2 pr-2">Time</th>
                <th class="text-left py-2 pr-2">Action</th>
                <th class="text-left py-2 pr-2">Good</th>
                <th class="text-left py-2 pr-2">Port</th>
                <th class="text-right py-2 pr-2">Qty</th>
                <th class="text-right py-2 pr-2">Unit</th>
                <th class="text-right py-2">Total</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="trade in trades"
                :key="trade.id"
                class="border-b border-slate-700/30 hover:bg-slate-700/20 transition-colors"
              >
                <td class="py-1.5 pr-2 text-xs text-slate-500 font-mono whitespace-nowrap">
                  {{ formatTime(trade.created_at) }}
                </td>
                <td class="py-1.5 pr-2">
                  <span
                    class="px-2 py-0.5 rounded-full text-[10px] font-medium uppercase"
                    :class="trade.action === 'buy'
                      ? 'bg-sky-500/20 text-sky-400'
                      : 'bg-emerald-500/20 text-emerald-400'"
                  >
                    {{ trade.action }}
                  </span>
                </td>
                <td class="py-1.5 pr-2 text-slate-300 text-xs">
                  {{ trade.good_name || trade.good_id?.substring(0, 8) || '---' }}
                </td>
                <td class="py-1.5 pr-2 text-slate-400 text-xs">{{ trade.port_name }}</td>
                <td class="py-1.5 pr-2 text-right text-slate-300 font-mono text-xs">{{ trade.quantity }}</td>
                <td class="py-1.5 pr-2 text-right text-slate-400 font-mono text-xs">{{ formatCurrency(trade.unit_price) }}</td>
                <td
                  class="py-1.5 text-right font-mono text-xs font-medium"
                  :class="trade.action === 'sell' ? 'text-emerald-400' : 'text-sky-400'"
                >
                  {{ formatCurrency(trade.total_price) }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
    </template>

    <!-- Passengers Tab -->
    <template v-if="activeTab === 'passengers'">
      <div v-if="passengersLoading && passengers.length === 0" class="flex items-center justify-center py-8">
        <Icon name="mdi:loading" class="animate-spin text-2xl text-slate-500" />
      </div>

      <div v-else-if="passengers.length === 0" class="text-center text-slate-600 text-sm py-8">
        No passenger boardings recorded yet
      </div>

      <template v-else>
        <!-- Passenger Summary -->
        <div class="grid grid-cols-3 gap-3 mb-3">
          <div class="bg-slate-900/50 rounded-lg p-2">
            <div class="text-[10px] text-slate-500 uppercase">Boardings</div>
            <div class="text-sm font-bold text-purple-400 font-mono">{{ passengers.length }}</div>
          </div>
          <div class="bg-slate-900/50 rounded-lg p-2">
            <div class="text-[10px] text-slate-500 uppercase">Passengers</div>
            <div class="text-sm font-bold text-purple-400 font-mono">{{ totalPassengerCount }}</div>
          </div>
          <div class="bg-slate-900/50 rounded-lg p-2">
            <div class="text-[10px] text-slate-500 uppercase">Revenue</div>
            <div class="text-sm font-bold text-emerald-400 font-mono">+{{ formatCurrency(totalPassengerRev) }}</div>
          </div>
        </div>

        <div class="max-h-72 overflow-y-auto pr-2">
          <table class="w-full text-sm">
            <thead class="sticky top-0 bg-slate-800 z-10">
              <tr class="text-xs text-slate-500 uppercase tracking-wide border-b border-slate-700">
                <th class="text-left py-2 pr-2">Time</th>
                <th class="text-right py-2 pr-2">Count</th>
                <th class="text-right py-2 pr-2">Bid</th>
                <th class="text-left py-2 pr-2">Route</th>
                <th class="text-left py-2">Ship</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="p in passengers"
                :key="p.id"
                class="border-b border-slate-700/30 hover:bg-slate-700/20 transition-colors"
              >
                <td class="py-1.5 pr-2 text-xs text-slate-500 font-mono whitespace-nowrap">
                  {{ formatTime(p.created_at) }}
                </td>
                <td class="py-1.5 pr-2 text-right text-slate-300 font-mono text-xs">{{ p.count }}</td>
                <td class="py-1.5 pr-2 text-right text-emerald-400 font-mono text-xs font-medium">
                  {{ formatCurrency(p.bid) }}
                </td>
                <td class="py-1.5 pr-2 text-xs">
                  <span class="text-slate-400">{{ p.origin_port_name }}</span>
                  <Icon name="lucide:arrow-right" class="text-[10px] text-slate-600 mx-1 inline" />
                  <span class="text-slate-300">{{ p.destination_port_name }}</span>
                </td>
                <td class="py-1.5 text-slate-400 text-xs">{{ p.ship_name }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
    </template>
  </div>
</template>

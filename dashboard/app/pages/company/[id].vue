<script setup lang="ts">
import type { Company } from '~/types'

const route = useRoute()
const router = useRouter()
const companyId = computed(() => Number(route.params.id))

const { companies, fetchCompanies } = useCompanies()
const { inventory, fetchInventory, startPolling: startInvPolling, stopPolling: stopInvPolling } = useInventory(companyId)
const { history, loading: pnlLoading, fetchHistory, connectSSE, disconnectSSE } = usePnL()
const { connect: connectEvents, disconnect: disconnectEvents } = useCompanyEvents()

const company = computed<Company | undefined>(() =>
  companies.value.find(c => c.id === companyId.value)
)

// Live treasury + ship count from inventory (polls every 10s from in-memory state).
const liveTreasury = computed(() => inventory.value?.treasury ?? company.value?.treasury ?? 0)
const liveShipCount = computed(() => inventory.value?.ships.length ?? 0)
const liveUpkeep = computed(() => inventory.value?.total_upkeep ?? 0)

// Redirect back if company not found after loading
watch([companies, companyId], () => {
  if (companies.value.length > 0 && !company.value) {
    router.push('/')
  }
})

// Event types that trigger inventory refresh (ships, cargo, warehouses).
const inventoryEvents = new Set([
  'ship_bought', 'ship_docked', 'ship_sailed', 'ship_sold',
  'trade', 'passenger', 'warehouse', 'economy',
])

// Single owner of fetch + SSE lifecycle
watch(companyId, (id) => {
  if (id) {
    startInvPolling(id, 30000)
    fetchHistory(id).then(() => connectSSE(id))

    // Connect real-time events SSE — trigger instant re-fetches.
    connectEvents(id, (event) => {
      if (inventoryEvents.has(event.type)) {
        fetchInventory(id)
      }
      if (event.type === 'economy' || event.type === 'ship_bought' || event.type === 'ship_sold') {
        fetchCompanies()
      }
    })
  }
}, { immediate: true })

onUnmounted(() => {
  disconnectSSE()
  disconnectEvents()
  stopInvPolling()
})

const latestPnL = computed(() => {
  if (history.value.length === 0) return null
  return history.value[history.value.length - 1]
})

function formatCurrency(value: number): string {
  return new Intl.NumberFormat('en-US').format(value)
}

function statusBadge(status: string): string {
  const m: Record<string, string> = {
    running: 'bg-emerald-500/20 text-emerald-400 border-emerald-500/30',
    paused: 'bg-yellow-500/20 text-yellow-400 border-yellow-500/30',
    error: 'bg-rose-500/20 text-rose-400 border-rose-500/30',
    bankrupt: 'bg-gray-500/20 text-gray-400 border-gray-500/30',
  }
  return m[status] || 'bg-gray-500/20 text-gray-400 border-gray-500/30'
}

function strategyBadge(strategy: string): string {
  const m: Record<string, string> = {
    arbitrage: 'bg-emerald-500/20 text-emerald-300',
    bulk_hauler: 'bg-blue-500/20 text-blue-300',
    market_maker: 'bg-amber-500/20 text-amber-300',
  }
  return m[strategy.toLowerCase()] || 'bg-purple-500/20 text-purple-300'
}
</script>

<template>
  <div v-if="company" class="space-y-8">
    <!-- Company Header -->
    <div class="flex items-start justify-between">
      <div>
        <div class="flex items-center gap-4 mb-1.5">
          <button
            class="text-slate-400 hover:text-slate-200 transition-colors p-1"
            @click="router.push('/')"
          >
            <Icon name="lucide:arrow-left" class="text-lg" />
          </button>
          <h2 class="text-3xl font-bold text-slate-100">{{ company.name }}</h2>
          <span class="px-3 py-1 rounded-full text-sm font-medium border" :class="statusBadge(company.status)">
            {{ company.status }}
          </span>
        </div>
        <div class="flex items-center gap-3 ml-11">
          <span class="font-mono text-base text-slate-400">{{ company.ticker }}</span>
          <span class="text-slate-600">|</span>
          <span class="px-2.5 py-1 rounded-full text-sm font-medium" :class="strategyBadge(company.strategy)">
            {{ company.strategy }}
          </span>
          <template v-if="company.agent_name && company.agent_name !== 'heuristic'">
            <span class="text-slate-600">|</span>
            <span class="px-2 py-0.5 rounded text-xs font-mono bg-violet-500/20 text-violet-300">{{ company.agent_name }}</span>
          </template>
          <span class="text-slate-600">|</span>
          <span class="text-sm text-slate-500 font-mono">{{ company.game_id }}</span>
        </div>
      </div>
    </div>

    <!-- Stats Bar -->
    <div class="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-6 gap-4">
      <div class="bg-slate-800 rounded-xl border border-slate-700 p-4">
        <div class="flex items-center gap-2 text-slate-400 text-sm mb-1.5">
          <Icon name="lucide:coins" class="text-amber-400" />
          Treasury
        </div>
        <div class="text-xl font-bold text-slate-100 font-mono">{{ formatCurrency(liveTreasury) }}</div>
        <div v-if="liveUpkeep" class="text-xs text-slate-500 font-mono mt-1">-{{ formatCurrency(liveUpkeep) }}/cycle</div>
      </div>
      <div class="bg-slate-800 rounded-xl border border-slate-700 p-4">
        <div class="flex items-center gap-2 text-slate-400 text-sm mb-1.5">
          <Icon name="mdi:ship" class="text-blue-400" />
          Ships
        </div>
        <div class="text-xl font-bold text-slate-100 font-mono">{{ liveShipCount || '---' }}</div>
      </div>
      <div class="bg-slate-800 rounded-xl border border-slate-700 p-4">
        <div class="flex items-center gap-2 text-slate-400 text-sm mb-1.5">
          <Icon name="lucide:trending-up" class="text-emerald-400" />
          Net P&L
        </div>
        <div class="text-xl font-bold font-mono" :class="(latestPnL?.net_pnl ?? 0) >= 0 ? 'text-emerald-400' : 'text-rose-400'">
          {{ latestPnL ? formatCurrency(latestPnL.net_pnl) : '---' }}
        </div>
      </div>
      <div class="bg-slate-800 rounded-xl border border-slate-700 p-4">
        <div class="flex items-center gap-2 text-slate-400 text-sm mb-1.5">
          <Icon name="lucide:arrow-up-right" class="text-emerald-400" />
          Revenue
        </div>
        <div class="text-xl font-bold text-emerald-400 font-mono">{{ latestPnL ? formatCurrency(latestPnL.total_rev) : '---' }}</div>
      </div>
      <div class="bg-slate-800 rounded-xl border border-slate-700 p-4">
        <div class="flex items-center gap-2 text-slate-400 text-sm mb-1.5">
          <Icon name="lucide:arrow-down-right" class="text-rose-400" />
          Costs
        </div>
        <div class="text-xl font-bold text-rose-400 font-mono">{{ latestPnL ? formatCurrency(latestPnL.total_costs) : '---' }}</div>
      </div>
      <div class="bg-slate-800 rounded-xl border border-slate-700 p-4">
        <div class="flex items-center gap-2 text-slate-400 text-sm mb-1.5">
          <Icon name="lucide:star" class="text-yellow-400" />
          Reputation
        </div>
        <div class="text-xl font-bold text-slate-100 font-mono">{{ company.reputation }}</div>
      </div>
    </div>

    <!-- P&L Charts (Treasury + P&L Breakdown) -->
    <PnLChart :history="history" :loading="pnlLoading" />

    <!-- Fleet & Trades (side by side) -->
    <div class="grid grid-cols-1 lg:grid-cols-2 gap-8">
      <FleetOverview :company-id="companyId" />
      <TradeHistory :company-id="companyId" />
    </div>

    <!-- Agent Decisions + Live Logs (side by side) -->
    <div class="grid grid-cols-1 lg:grid-cols-2 gap-8">
      <AgentDecisions :company-id="companyId" />
      <LiveLogs :company-id="companyId" />
    </div>
  </div>

  <!-- Loading state -->
  <div v-else class="flex items-center justify-center py-20 text-slate-600">
    <Icon name="mdi:loading" class="animate-spin text-4xl mr-3" />
    <span>Loading company...</span>
  </div>
</template>

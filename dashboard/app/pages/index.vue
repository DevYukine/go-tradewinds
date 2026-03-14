<script setup lang="ts">
import type { Company } from '~/types'

const { companies, companiesByStrategy, fetchCompanies } = useCompanies()
const router = useRouter()

// Use the shared inventory composable — same global state as company detail page.
const { inventories, fetchInventory } = useInventory()

async function fetchAllInventories() {
  // Fetch all inventories concurrently (not sequentially) to avoid slow waterfalls.
  await Promise.allSettled(
    companies.value.map(c => fetchInventory(c.id))
  )
}

// Initial fetch when companies arrive, then poll.
// No SSE on the overview page — opening 7 SSE connections (one per company)
// would exceed the browser's 6-connection-per-origin HTTP/1.1 limit and block
// all other API requests.
let initialFetchDone = false
watch(
  () => companies.value,
  (list) => {
    if (list.length > 0 && !initialFetchDone) {
      initialFetchDone = true
      fetchAllInventories()
    }
  },
  { immediate: true }
)

let pollTimer: ReturnType<typeof setInterval> | null = null
onMounted(() => { pollTimer = setInterval(fetchAllInventories, 15000) })
onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})

function selectCompany(company: Company) {
  router.push(`/company/${company.id}`)
}

// Aggregates — prefer live inventory treasury over stale DB treasury.
const totalTreasury = computed(() =>
  companies.value.reduce((s, c) => s + (inventories.value[c.id]?.treasury ?? c.treasury), 0)
)
const totalShips = computed(() => Object.values(inventories.value).reduce((s, i) => s + i.ships.length, 0))
const totalUpkeep = computed(() => Object.values(inventories.value).reduce((s, i) => s + i.total_upkeep, 0))
const shipsAtSea = computed(() => Object.values(inventories.value).reduce((s, i) => s + i.ships.filter(sh => sh.status === 'traveling').length, 0))
const shipsDocked = computed(() => Object.values(inventories.value).reduce((s, i) => s + i.ships.filter(sh => sh.status === 'docked').length, 0))
const statusCounts = computed(() => {
  const c: Record<string, number> = { running: 0, paused: 0, error: 0, bankrupt: 0 }
  companies.value.forEach(co => c[co.status] = (c[co.status] || 0) + 1)
  return c
})

function formatCurrency(value: number): string {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`
  if (value >= 1_000) return `${(value / 1_000).toFixed(1)}K`
  return new Intl.NumberFormat('en-US').format(value)
}

function statusDot(status: string): string {
  const m: Record<string, string> = { running: 'bg-emerald-500', paused: 'bg-yellow-500', error: 'bg-rose-500', bankrupt: 'bg-gray-500' }
  return m[status] || 'bg-gray-500'
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
  <div class="space-y-6">
    <!-- Page Header -->
    <div>
      <h2 class="text-2xl font-bold text-slate-100">Fleet Overview</h2>
      <p class="text-sm text-slate-500 mt-1">All companies at a glance</p>
    </div>

    <!-- Aggregate Stats -->
    <div class="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-6 gap-3">
      <div class="bg-slate-800 rounded-lg border border-slate-700 p-4">
        <div class="flex items-center gap-2 text-slate-400 text-xs mb-1">
          <Icon name="lucide:building-2" class="text-slate-500" />
          Companies
        </div>
        <div class="text-2xl font-bold text-slate-100 font-mono">{{ companies.length }}</div>
        <div class="text-[10px] text-slate-500 mt-1">
          <span class="text-emerald-400">{{ statusCounts.running }}</span> running
          <template v-if="statusCounts.error > 0">
            / <span class="text-rose-400">{{ statusCounts.error }}</span> error
          </template>
        </div>
      </div>

      <div class="bg-slate-800 rounded-lg border border-slate-700 p-4">
        <div class="flex items-center gap-2 text-slate-400 text-xs mb-1">
          <Icon name="lucide:coins" class="text-amber-400" />
          Total Treasury
        </div>
        <div class="text-2xl font-bold text-amber-400 font-mono">{{ formatCurrency(totalTreasury) }}</div>
      </div>

      <div class="bg-slate-800 rounded-lg border border-slate-700 p-4">
        <div class="flex items-center gap-2 text-slate-400 text-xs mb-1">
          <Icon name="mdi:ship" class="text-sky-400" />
          Total Ships
        </div>
        <div class="text-2xl font-bold text-sky-400 font-mono">{{ totalShips }}</div>
        <div class="text-[10px] text-slate-500 mt-1">
          <span class="text-emerald-400">{{ shipsDocked }}</span> docked /
          <span class="text-sky-400">{{ shipsAtSea }}</span> sailing
        </div>
      </div>

      <div class="bg-slate-800 rounded-lg border border-slate-700 p-4">
        <div class="flex items-center gap-2 text-slate-400 text-xs mb-1">
          <Icon name="lucide:arrow-up-circle" class="text-rose-400" />
          Total Upkeep
        </div>
        <div class="text-2xl font-bold text-rose-400 font-mono">{{ formatCurrency(totalUpkeep) }}</div>
        <div class="text-[10px] text-slate-500 mt-1">per hour</div>
      </div>

      <div class="bg-slate-800 rounded-lg border border-slate-700 p-4">
        <div class="flex items-center gap-2 text-slate-400 text-xs mb-1">
          <Icon name="lucide:layers" class="text-purple-400" />
          Strategies
        </div>
        <div class="text-2xl font-bold text-purple-400 font-mono">{{ Object.keys(companiesByStrategy).length }}</div>
        <div class="text-[10px] text-slate-500 mt-1 truncate">{{ Object.keys(companiesByStrategy).join(', ') }}</div>
      </div>

      <div class="bg-slate-800 rounded-lg border border-slate-700 p-4">
        <div class="flex items-center gap-2 text-slate-400 text-xs mb-1">
          <Icon name="lucide:star" class="text-yellow-400" />
          Avg Reputation
        </div>
        <div class="text-2xl font-bold text-yellow-400 font-mono">
          {{ companies.length > 0 ? Math.round(companies.reduce((s, c) => s + c.reputation, 0) / companies.length) : 0 }}
        </div>
      </div>
    </div>

    <!-- Company Cards by Strategy -->
    <div
      v-for="(stratCompanies, strategy) in companiesByStrategy"
      :key="strategy"
    >
      <div class="flex items-center gap-2 mb-3">
        <span class="px-2.5 py-0.5 rounded-full text-xs font-semibold uppercase" :class="strategyBadge(strategy as string)">
          {{ strategy }}
        </span>
        <span class="text-xs text-slate-500">{{ stratCompanies.length }} {{ stratCompanies.length === 1 ? 'company' : 'companies' }}</span>
      </div>

      <div class="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-4 mb-6">
        <button
          v-for="company in stratCompanies"
          :key="company.id"
          class="bg-slate-800 rounded-lg border border-slate-700 p-4 text-left hover:border-slate-500 transition-all group"
          @click="selectCompany(company)"
        >
          <div class="flex items-center justify-between mb-3">
            <div class="flex items-center gap-2.5">
              <span class="w-2.5 h-2.5 rounded-full flex-shrink-0" :class="statusDot(company.status)" />
              <span class="text-sm font-semibold text-slate-200 group-hover:text-slate-100">{{ company.name }}</span>
              <span class="text-xs text-slate-500 font-mono">{{ company.ticker }}</span>
            </div>
            <span class="px-2 py-0.5 rounded-full text-[10px] font-medium border" :class="statusBadge(company.status)">
              {{ company.status }}
            </span>
          </div>

          <div class="grid grid-cols-3 gap-3">
            <div>
              <div class="text-[10px] text-slate-500 uppercase tracking-wide">Treasury</div>
              <div class="text-sm font-bold text-amber-400 font-mono">{{ formatCurrency(inventories[company.id]?.treasury ?? company.treasury) }}</div>
            </div>
            <div>
              <div class="text-[10px] text-slate-500 uppercase tracking-wide">Ships</div>
              <div class="text-sm font-bold text-sky-400 font-mono">{{ inventories[company.id]?.ships.length ?? '---' }}</div>
            </div>
            <div>
              <div class="text-[10px] text-slate-500 uppercase tracking-wide">Reputation</div>
              <div class="text-sm font-bold text-yellow-400 font-mono">{{ company.reputation }}</div>
            </div>
          </div>

          <!-- Mini ship indicators -->
          <div v-if="inventories[company.id]?.ships.length" class="mt-3">
            <div class="flex items-center gap-2 text-[10px] text-slate-500 mb-1">
              <span><span class="text-emerald-400">{{ inventories[company.id]?.ships.filter(s => s.status === 'docked').length }}</span> docked</span>
              <span><span class="text-sky-400">{{ inventories[company.id]?.ships.filter(s => s.status === 'traveling').length }}</span> sailing</span>
              <span v-if="inventories[company.id]?.total_upkeep">
                upkeep <span class="text-rose-400 font-mono">{{ formatCurrency(inventories[company.id]!.total_upkeep) }}/hr</span>
              </span>
            </div>
            <div class="flex gap-1 flex-wrap">
              <div
                v-for="ship in inventories[company.id]?.ships"
                :key="ship.ship_id"
                class="w-6 h-6 rounded flex items-center justify-center text-[10px]"
                :class="ship.status === 'docked' ? 'bg-emerald-500/20 text-emerald-400' : 'bg-sky-500/20 text-sky-400'"
                :title="`${ship.ship_name} - ${ship.status}${ship.port_name ? ' at ' + ship.port_name : ''}`"
              >
                <Icon :name="ship.status === 'docked' ? 'lucide:anchor' : 'mdi:sail-boat'" class="text-xs" />
              </div>
            </div>
          </div>

          <div class="mt-3 text-[10px] text-slate-600 group-hover:text-slate-400 transition-colors flex items-center gap-1">
            <Icon name="lucide:arrow-right" class="text-[10px]" />
            View details
          </div>
        </button>
      </div>
    </div>

    <div v-if="companies.length === 0" class="flex flex-col items-center justify-center py-20 text-slate-600">
      <Icon name="mdi:loading" class="animate-spin text-4xl mb-4" />
      <p>Loading companies...</p>
    </div>
  </div>
</template>

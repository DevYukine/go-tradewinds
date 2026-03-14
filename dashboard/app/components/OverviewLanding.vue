<script setup lang="ts">
import type { Company, CompanyInventory } from '~/types'

const props = defineProps<{
  companies: Company[]
  companiesByStrategy: Record<string, Company[]>
}>()

const emit = defineEmits<{
  select: [company: Company]
}>()

const config = useRuntimeConfig()
const apiBase = config.public.apiBase

// Fetch inventory for each company to get ship counts and upkeep.
const inventories = ref<Record<number, CompanyInventory>>({})

async function fetchAllInventories() {
  for (const company of props.companies) {
    try {
      const inv = await $fetch<CompanyInventory>(
        `${apiBase}/api/companies/${company.id}/inventory`
      )
      inventories.value[company.id] = inv
    } catch {
      // Company may not be running yet.
    }
  }
}

watch(
  () => props.companies,
  (companies) => {
    if (companies.length > 0) fetchAllInventories()
  },
  { immediate: true }
)

let pollTimer: ReturnType<typeof setInterval> | null = null
onMounted(() => {
  pollTimer = setInterval(fetchAllInventories, 15000)
})
onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})

// Aggregate stats
const totalTreasury = computed(() =>
  props.companies.reduce((sum, c) => sum + c.treasury, 0)
)

const totalShips = computed(() =>
  Object.values(inventories.value).reduce((sum, inv) => sum + inv.ships.length, 0)
)

const totalUpkeep = computed(() =>
  Object.values(inventories.value).reduce((sum, inv) => sum + inv.total_upkeep, 0)
)

const shipsAtSea = computed(() =>
  Object.values(inventories.value).reduce(
    (sum, inv) => sum + inv.ships.filter(s => s.status === 'sailing').length,
    0
  )
)

const shipsDocked = computed(() =>
  Object.values(inventories.value).reduce(
    (sum, inv) => sum + inv.ships.filter(s => s.status === 'docked').length,
    0
  )
)

const statusCounts = computed(() => {
  const counts: Record<string, number> = { running: 0, paused: 0, error: 0, bankrupt: 0 }
  for (const c of props.companies) {
    counts[c.status] = (counts[c.status] || 0) + 1
  }
  return counts
})

function formatCurrency(value: number): string {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`
  if (value >= 1_000) return `${(value / 1_000).toFixed(1)}K`
  return new Intl.NumberFormat('en-US').format(value)
}

function statusClasses(status: Company['status']): string {
  switch (status) {
    case 'running': return 'bg-emerald-500/20 text-emerald-400 border-emerald-500/30'
    case 'paused': return 'bg-yellow-500/20 text-yellow-400 border-yellow-500/30'
    case 'error': return 'bg-rose-500/20 text-rose-400 border-rose-500/30'
    case 'bankrupt': return 'bg-gray-500/20 text-gray-400 border-gray-500/30'
    default: return 'bg-gray-500/20 text-gray-400 border-gray-500/30'
  }
}

function statusDot(status: Company['status']): string {
  switch (status) {
    case 'running': return 'bg-emerald-500'
    case 'paused': return 'bg-yellow-500'
    case 'error': return 'bg-rose-500'
    case 'bankrupt': return 'bg-gray-500'
    default: return 'bg-gray-500'
  }
}

function strategyBadgeClasses(strategy: string): string {
  const colors: Record<string, string> = {
    arbitrage: 'bg-emerald-500/20 text-emerald-300',
    bulk_hauler: 'bg-blue-500/20 text-blue-300',
    market_maker: 'bg-amber-500/20 text-amber-300',
  }
  return colors[strategy.toLowerCase()] || 'bg-purple-500/20 text-purple-300'
}
</script>

<template>
  <div class="space-y-6">
    <!-- Header -->
    <div>
      <h2 class="text-2xl font-bold text-slate-100">Fleet Overview</h2>
      <p class="text-sm text-slate-500 mt-1">All companies at a glance</p>
    </div>

    <!-- Aggregate Stats -->
    <div class="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-6 gap-3">
      <div class="bg-slate-800 rounded-lg border border-slate-700 p-4">
        <div class="flex items-center gap-2 text-slate-400 text-xs mb-1">
          <Icon name="lucide:building-2" class="text-slate-500" />
          Companies
        </div>
        <div class="text-2xl font-bold text-slate-100 font-mono">{{ companies.length }}</div>
        <div class="text-[10px] text-slate-500 mt-1">
          <span class="text-emerald-400">{{ statusCounts.running }}</span> running
          <template v-if="statusCounts.error > 0">
            <span class="text-slate-600 mx-0.5">/</span>
            <span class="text-rose-400">{{ statusCounts.error }}</span> error
          </template>
        </div>
      </div>

      <div class="bg-slate-800 rounded-lg border border-slate-700 p-4">
        <div class="flex items-center gap-2 text-slate-400 text-xs mb-1">
          <Icon name="lucide:coins" class="text-amber-400" />
          Total Treasury
        </div>
        <div class="text-2xl font-bold text-amber-400 font-mono">{{ formatCurrency(totalTreasury) }}</div>
        <div class="text-[10px] text-slate-500 mt-1">across all companies</div>
      </div>

      <div class="bg-slate-800 rounded-lg border border-slate-700 p-4">
        <div class="flex items-center gap-2 text-slate-400 text-xs mb-1">
          <Icon name="mdi:ship" class="text-sky-400" />
          Total Ships
        </div>
        <div class="text-2xl font-bold text-sky-400 font-mono">{{ totalShips }}</div>
        <div class="text-[10px] text-slate-500 mt-1">
          <span class="text-emerald-400">{{ shipsDocked }}</span> docked
          <span class="text-slate-600 mx-0.5">/</span>
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
        <div class="text-[10px] text-slate-500 mt-1">
          {{ Object.keys(companiesByStrategy).join(', ') }}
        </div>
      </div>

      <div class="bg-slate-800 rounded-lg border border-slate-700 p-4">
        <div class="flex items-center gap-2 text-slate-400 text-xs mb-1">
          <Icon name="lucide:star" class="text-yellow-400" />
          Avg Reputation
        </div>
        <div class="text-2xl font-bold text-yellow-400 font-mono">
          {{ companies.length > 0 ? Math.round(companies.reduce((s, c) => s + c.reputation, 0) / companies.length) : 0 }}
        </div>
        <div class="text-[10px] text-slate-500 mt-1">across all companies</div>
      </div>
    </div>

    <!-- Strategy Groups -->
    <div
      v-for="(stratCompanies, strategy) in companiesByStrategy"
      :key="strategy"
    >
      <div class="flex items-center gap-2 mb-3">
        <span
          class="px-2.5 py-0.5 rounded-full text-xs font-semibold uppercase"
          :class="strategyBadgeClasses(strategy as string)"
        >
          {{ strategy }}
        </span>
        <span class="text-xs text-slate-500">{{ stratCompanies.length }} {{ stratCompanies.length === 1 ? 'company' : 'companies' }}</span>
      </div>

      <div class="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-4 mb-6">
        <button
          v-for="company in stratCompanies"
          :key="company.id"
          class="bg-slate-800 rounded-lg border border-slate-700 p-4 text-left hover:border-slate-500 hover:bg-slate-750 transition-all group"
          @click="emit('select', company)"
        >
          <!-- Company Header -->
          <div class="flex items-center justify-between mb-3">
            <div class="flex items-center gap-2.5">
              <span
                class="w-2.5 h-2.5 rounded-full flex-shrink-0"
                :class="statusDot(company.status)"
              />
              <div>
                <span class="text-sm font-semibold text-slate-200 group-hover:text-slate-100">
                  {{ company.name }}
                </span>
                <span class="text-xs text-slate-500 ml-2 font-mono">{{ company.ticker }}</span>
              </div>
            </div>
            <span
              class="px-2 py-0.5 rounded-full text-[10px] font-medium border"
              :class="statusClasses(company.status)"
            >
              {{ company.status }}
            </span>
          </div>

          <!-- Stats Grid -->
          <div class="grid grid-cols-3 gap-3">
            <div>
              <div class="text-[10px] text-slate-500 uppercase tracking-wide">Treasury</div>
              <div class="text-sm font-bold text-amber-400 font-mono">{{ formatCurrency(company.treasury) }}</div>
            </div>
            <div>
              <div class="text-[10px] text-slate-500 uppercase tracking-wide">Ships</div>
              <div class="text-sm font-bold text-sky-400 font-mono">
                {{ inventories[company.id]?.ships.length ?? '---' }}
              </div>
            </div>
            <div>
              <div class="text-[10px] text-slate-500 uppercase tracking-wide">Reputation</div>
              <div class="text-sm font-bold text-yellow-400 font-mono">{{ company.reputation }}</div>
            </div>
          </div>

          <!-- Ship Status Bar -->
          <div v-if="inventories[company.id]?.ships.length" class="mt-3">
            <div class="flex items-center gap-2 text-[10px] text-slate-500 mb-1">
              <span>
                <span class="text-emerald-400">{{ inventories[company.id]?.ships.filter(s => s.status === 'docked').length }}</span> docked
              </span>
              <span>
                <span class="text-sky-400">{{ inventories[company.id]?.ships.filter(s => s.status === 'sailing').length }}</span> sailing
              </span>
              <span v-if="inventories[company.id]?.total_upkeep">
                upkeep <span class="text-rose-400 font-mono">{{ formatCurrency(inventories[company.id]!.total_upkeep) }}/hr</span>
              </span>
            </div>
            <!-- Mini ship indicators -->
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

          <!-- Warehouses summary -->
          <div v-if="inventories[company.id]?.warehouses.length" class="mt-2 flex items-center gap-1 text-[10px] text-slate-500">
            <Icon name="lucide:warehouse" class="text-purple-400" />
            {{ inventories[company.id]?.warehouses.length }} {{ inventories[company.id]?.warehouses.length === 1 ? 'warehouse' : 'warehouses' }}
          </div>

          <!-- Click hint -->
          <div class="mt-3 text-[10px] text-slate-600 group-hover:text-slate-400 transition-colors flex items-center gap-1">
            <Icon name="lucide:arrow-right" class="text-[10px]" />
            View details
          </div>
        </button>
      </div>
    </div>

    <!-- Strategy Comparison + Optimizer at bottom -->
    <div class="grid grid-cols-1 lg:grid-cols-2 gap-6">
      <StrategyComparison />
      <OptimizerLog />
    </div>
  </div>
</template>

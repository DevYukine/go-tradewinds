<script setup lang="ts">
import type { Company, CompanyInventory } from '~/types'

const { companies, fetchCompanies } = useCompanies()
const { world } = useWorld()
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
// Component-scoped flag so navigating away and back triggers a re-fetch.
const initialFetchDone = ref(false)
watch(
  () => companies.value,
  (list) => {
    if (list.length > 0 && !initialFetchDone.value) {
      initialFetchDone.value = true
      fetchAllInventories()
    }
  },
  { immediate: true }
)

let pollTimer: ReturnType<typeof setInterval> | null = null
onMounted(() => {
  pollTimer = setInterval(() => {
    fetchCompanies()
    fetchAllInventories()
  }, 15000)
})
onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})

function selectCompany(company: Company) {
  router.push(`/company/${company.id}`)
}

// --- Aggregates ---

const allInventories = computed(() => Object.values(inventories.value) as CompanyInventory[])

const totalTreasury = computed(() =>
  companies.value.reduce((s, c) => s + (inventories.value[c.id]?.treasury ?? c.treasury), 0)
)
const totalShips = computed(() => allInventories.value.reduce((s, i) => s + i.ships.length, 0))
const totalUpkeep = computed(() => allInventories.value.reduce((s, i) => s + i.total_upkeep, 0))
const shipsAtSea = computed(() => allInventories.value.reduce((s, i) => s + i.ships.filter(sh => sh.status === 'traveling').length, 0))
const shipsDocked = computed(() => allInventories.value.reduce((s, i) => s + i.ships.filter(sh => sh.status === 'docked').length, 0))
const statusCounts = computed(() => {
  const c: Record<string, number> = { running: 0, paused: 0, error: 0, bankrupt: 0 }
  companies.value.forEach(co => c[co.status] = (c[co.status] || 0) + 1)
  return c
})

// Warehouse stats.
const totalWarehouses = computed(() => allInventories.value.reduce((s, i) => s + i.warehouses.length, 0))
const totalWarehouseItems = computed(() =>
  allInventories.value.reduce((s, i) => s + i.warehouses.reduce((ws, w) => ws + w.items.reduce((is, item) => is + item.quantity, 0), 0), 0)
)
function companyWarehouseCount(id: number): number {
  return inventories.value[id]?.warehouses.length ?? 0
}

// Cargo value — sum of buy-price-based cargo valuations across all ships.
const totalCargoValue = computed(() =>
  allInventories.value.reduce((s, i) => s + (i.cargo_value ?? 0), 0)
)
const totalCargoItems = computed(() =>
  allInventories.value.reduce((s, i) => s + i.ships.reduce((ss, sh) => ss + sh.cargo_total, 0), 0)
)

// Fleet value — look up base_price from world data ship types by matching ship_type name.
const shipTypePrices = computed(() => {
  const m: Record<string, number> = {}
  if (world.value?.ship_types) {
    for (const st of world.value.ship_types) {
      m[st.name] = st.base_price
    }
  }
  return m
})

const fleetValue = computed(() =>
  allInventories.value.reduce((s, i) => s + i.ships.reduce((ss, sh) => ss + (shipTypePrices.value[sh.ship_type] ?? 0), 0), 0)
)

// Runway — how many hours the treasury can sustain current upkeep.
// Upkeep is charged per 5-hour cycle, so multiply cycles by 5 for hours.
const UPKEEP_CYCLE_HOURS = 5
const runwayHours = computed(() => {
  if (totalUpkeep.value === 0) return Infinity
  return (totalTreasury.value / totalUpkeep.value) * UPKEEP_CYCLE_HOURS
})

// Total assets = treasury + fleet value + cargo value.
const totalAssets = computed(() => totalTreasury.value + fleetValue.value + totalCargoValue.value)

// Financial overview bar segments.
const barSegments = computed(() => {
  const total = totalAssets.value || 1
  const treasury = totalTreasury.value
  const fleet = fleetValue.value
  const cargo = totalCargoValue.value
  return {
    treasury: (treasury / total) * 100,
    fleet: (fleet / total) * 100,
    cargo: (cargo / total) * 100,
  }
})

// Max treasury across companies (for treasury bar in company cards).
const maxTreasury = computed(() =>
  Math.max(...companies.value.map(c => inventories.value[c.id]?.treasury ?? c.treasury), 1)
)

// Company upkeep helper.
function companyUpkeep(id: number): number {
  return inventories.value[id]?.total_upkeep ?? 0
}

function companyTreasury(c: Company): number {
  return inventories.value[c.id]?.treasury ?? c.treasury
}

function companyShipCount(id: number): number {
  return inventories.value[id]?.ships.length ?? 0
}

function treasuryBarPct(c: Company): number {
  return (companyTreasury(c) / maxTreasury.value) * 100
}

function formatCurrency(value: number): string {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`
  if (value >= 1_000) return `${(value / 1_000).toFixed(1)}K`
  return new Intl.NumberFormat('en-US').format(value)
}

function formatFull(value: number): string {
  return new Intl.NumberFormat('en-US').format(value)
}

function formatRunway(hours: number): string {
  if (hours === Infinity) return '---'
  if (hours >= 24) return `${Math.floor(hours / 24)}d ${Math.floor(hours % 24)}h`
  return `${Math.floor(hours)}h`
}

function statusDot(status: string): string {
  const m: Record<string, string> = { running: 'bg-emerald-500', paused: 'bg-yellow-500', error: 'bg-rose-500', bankrupt: 'bg-rose-600 animate-pulse' }
  return m[status] || 'bg-gray-500'
}

function strategyColor(strategy: string): string {
  const m: Record<string, string> = {
    arbitrage: 'text-emerald-400',
    bulk_hauler: 'text-sky-400',
    market_maker: 'text-amber-400',
  }
  return m[strategy.toLowerCase()] || 'text-purple-400'
}

// Per-company ship status counts.
function companyDockedCount(id: number): number {
  return inventories.value[id]?.ships.filter(s => s.status === 'docked').length ?? 0
}

function companySailingCount(id: number): number {
  return inventories.value[id]?.ships.filter(s => s.status === 'traveling').length ?? 0
}
</script>

<template>
  <div class="space-y-8">
    <!-- Page Header -->
    <div class="flex items-center justify-between">
      <div>
        <h2 class="text-3xl font-bold text-slate-100">Dashboard</h2>
        <p class="text-sm text-slate-500 mt-1">Fleet overview across {{ companies.length }} companies</p>
      </div>
      <button
        class="flex items-center gap-2 text-sm text-slate-400 hover:text-slate-200 transition-colors bg-slate-800 hover:bg-slate-700 border border-slate-700 rounded-lg px-4 py-2"
        @click="fetchCompanies(); fetchAllInventories()"
      >
        <Icon name="lucide:refresh-cw" class="text-sm" />
        Refresh
      </button>
    </div>

    <!-- Financial Overview -->
    <div class="bg-slate-800/80 rounded-xl border border-slate-700 p-6">
      <div class="flex items-center justify-between mb-5">
        <h3 class="text-sm font-semibold text-slate-400 uppercase tracking-wider">Financial Overview</h3>
        <div class="text-sm text-slate-500">
          Total Assets
          <span class="text-slate-200 font-mono font-bold ml-2">{{ formatFull(totalAssets) }}g</span>
        </div>
      </div>

      <!-- Stacked bar -->
      <div class="w-full h-3 rounded-full bg-slate-700 overflow-hidden flex mb-4">
        <div
          class="h-full bg-sky-500 transition-all duration-500"
          :style="{ width: barSegments.treasury + '%' }"
        />
        <div
          class="h-full bg-amber-500 transition-all duration-500"
          :style="{ width: barSegments.cargo + '%' }"
        />
        <div
          class="h-full bg-slate-500 transition-all duration-500"
          :style="{ width: barSegments.fleet + '%' }"
        />
      </div>

      <!-- Legend -->
      <div class="flex items-center gap-6 mb-6 text-xs text-slate-500">
        <span class="flex items-center gap-1.5">
          <span class="w-2.5 h-2.5 rounded-full bg-sky-500" />
          Cash
        </span>
        <span class="flex items-center gap-1.5">
          <span class="w-2.5 h-2.5 rounded-full bg-amber-500" />
          Cargo
        </span>
        <span class="flex items-center gap-1.5">
          <span class="w-2.5 h-2.5 rounded-full bg-slate-500" />
          Fleet
        </span>
      </div>

      <!-- Breakdown values -->
      <div class="grid grid-cols-2 md:grid-cols-4 xl:grid-cols-6 gap-6">
        <div class="text-center">
          <div class="text-xs text-slate-500 mb-1">Treasury</div>
          <div class="text-xl font-bold text-sky-400 font-mono">{{ formatFull(totalTreasury) }}g</div>
        </div>
        <div class="text-center">
          <div class="text-xs text-slate-500 mb-1">Fleet Value</div>
          <div class="text-xl font-bold text-slate-300 font-mono">{{ formatFull(fleetValue) }}g</div>
          <div class="text-[10px] text-slate-600">{{ totalShips }} ships</div>
        </div>
        <div class="text-center">
          <div class="text-xs text-slate-500 mb-1">Cargo Value</div>
          <div class="text-xl font-bold text-amber-400 font-mono">{{ formatFull(totalCargoValue) }}g</div>
          <div class="text-[10px] text-slate-600">{{ formatFull(totalCargoItems) }} items aboard</div>
        </div>
        <div v-if="totalWarehouses > 0" class="text-center">
          <div class="text-xs text-slate-500 mb-1">Warehouses</div>
          <div class="text-xl font-bold text-purple-400 font-mono">{{ totalWarehouses }}</div>
          <div class="text-[10px] text-slate-600">{{ formatFull(totalWarehouseItems) }} items stored</div>
        </div>
        <div class="text-center">
          <div class="text-xs text-slate-500 mb-1">Upkeep / Cycle</div>
          <div class="text-xl font-bold text-rose-400 font-mono">{{ formatFull(totalUpkeep) }}g</div>
        </div>
        <div class="text-center">
          <div class="text-xs text-slate-500 mb-1">Runway</div>
          <div class="text-xl font-bold text-emerald-400 font-mono">{{ formatRunway(runwayHours) }}</div>
          <div class="text-[10px] text-slate-600">at current burn</div>
        </div>
        <div class="text-center">
          <div class="text-xs text-slate-500 mb-1">Avg Treasury</div>
          <div class="text-xl font-bold text-slate-300 font-mono">{{ companies.length > 0 ? formatFull(Math.round(totalTreasury / companies.length)) + 'g' : '---' }}</div>
          <div class="text-[10px] text-slate-600">per company</div>
        </div>
      </div>
    </div>

    <!-- Stats Row -->
    <div class="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-6 gap-4">
      <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
        <div class="flex items-center gap-2 text-slate-400 text-sm mb-1.5">
          <Icon name="lucide:building-2" class="text-slate-500" />
          Companies
        </div>
        <div class="text-3xl font-bold text-slate-100 font-mono">{{ companies.length }}</div>
        <div class="text-xs text-slate-500 mt-1.5">
          <span class="text-emerald-400">{{ statusCounts.running }}</span> active
          <template v-if="statusCounts.bankrupt > 0">
            / <span class="text-gray-400">{{ statusCounts.bankrupt }}</span> bankrupt
          </template>
          <template v-if="statusCounts.error > 0">
            / <span class="text-rose-400">{{ statusCounts.error }}</span> error
          </template>
        </div>
      </div>

      <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
        <div class="flex items-center gap-2 text-slate-400 text-sm mb-1.5">
          <Icon name="lucide:coins" class="text-amber-400" />
          Treasury
        </div>
        <div class="text-3xl font-bold text-amber-400 font-mono">{{ formatCurrency(totalTreasury) }}</div>
        <div class="text-xs text-slate-500 mt-1.5">
          avg {{ companies.length > 0 ? formatCurrency(Math.round(totalTreasury / companies.length)) : '0' }}
        </div>
      </div>

      <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
        <div class="flex items-center gap-2 text-slate-400 text-sm mb-1.5">
          <Icon name="mdi:ship" class="text-sky-400" />
          Ships
        </div>
        <div class="text-3xl font-bold text-sky-400 font-mono">{{ totalShips }}</div>
        <div class="text-xs text-slate-500 mt-1.5">
          <span class="text-emerald-400">{{ shipsDocked }}</span> docked /
          <span class="text-sky-400">{{ shipsAtSea }}</span> sailing
        </div>
      </div>

      <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
        <div class="flex items-center gap-2 text-slate-400 text-sm mb-1.5">
          <Icon name="lucide:anchor" class="text-purple-400" />
          Fleet Value
        </div>
        <div class="text-3xl font-bold text-purple-400 font-mono">{{ formatCurrency(fleetValue) }}</div>
      </div>

      <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
        <div class="flex items-center gap-2 text-slate-400 text-sm mb-1.5">
          <Icon name="lucide:arrow-up-circle" class="text-rose-400" />
          Upkeep/Cycle
        </div>
        <div class="text-3xl font-bold text-rose-400 font-mono">{{ formatCurrency(totalUpkeep) }}</div>
        <div class="text-xs text-slate-500 mt-1.5">~{{ formatCurrency(Math.round(totalUpkeep * 24 / UPKEEP_CYCLE_HOURS)) }}/day</div>
      </div>

      <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
        <div class="flex items-center gap-2 text-slate-400 text-sm mb-1.5">
          <Icon name="lucide:clock" class="text-emerald-400" />
          Runway
        </div>
        <div class="text-3xl font-bold text-emerald-400 font-mono">{{ formatRunway(runwayHours) }}</div>
        <div class="text-xs text-slate-500 mt-1.5">at current burn</div>
      </div>
    </div>

    <!-- Companies Grid -->
    <div>
      <h3 class="text-sm font-semibold text-slate-400 uppercase tracking-wider mb-4">Companies</h3>

      <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4 2xl:grid-cols-5 gap-5">
        <button
          v-for="company in companies"
          :key="company.id"
          class="relative bg-slate-800 rounded-xl border p-5 text-left hover:border-slate-500 transition-all group"
          :class="company.status === 'bankrupt' ? 'border-rose-900/60 opacity-75' : 'border-slate-700'"
          @click="selectCompany(company)"
        >
          <!-- Bankruptcy overlay label -->
          <div
            v-if="company.status === 'bankrupt'"
            class="absolute top-0 right-0 bg-rose-600 text-white text-[10px] font-bold uppercase tracking-wider px-2 py-0.5 rounded-bl-lg rounded-tr-xl"
          >
            Bankrupt
          </div>

          <!-- Name + Status -->
          <div class="flex items-start justify-between mb-1">
            <div>
              <div class="text-base font-semibold group-hover:text-slate-100" :class="company.status === 'bankrupt' ? 'text-slate-400' : 'text-slate-200'">{{ company.name }}</div>
              <div class="text-xs text-slate-500 font-mono">{{ company.ticker }}</div>
            </div>
            <span class="w-2.5 h-2.5 rounded-full mt-1.5 flex-shrink-0" :class="statusDot(company.status)" />
          </div>

          <!-- Strategy + Agent badges -->
          <div class="flex items-center gap-2 mb-3">
            <span class="text-xs font-medium" :class="strategyColor(company.strategy)">{{ company.strategy }}</span>
            <span
              v-if="company.agent_name && company.agent_name !== 'heuristic'"
              class="text-[10px] font-mono px-1.5 py-0.5 rounded bg-violet-500/20 text-violet-300 truncate max-w-[140px]"
              :title="company.agent_name"
            >{{ company.agent_name.replace(/^llm:/, '') }}</span>
          </div>

          <!-- Treasury + Bar -->
          <div class="mb-3">
            <div class="flex items-center justify-between mb-1.5">
              <span class="text-xs text-slate-500">Treasury</span>
              <span class="text-sm font-bold font-mono" :class="company.status === 'bankrupt' ? 'text-rose-400' : 'text-amber-400'">{{ formatCurrency(companyTreasury(company)) }}</span>
            </div>
            <div class="w-full h-1.5 rounded-full bg-slate-700 overflow-hidden">
              <div
                class="h-full rounded-full transition-all duration-500"
                :class="company.status === 'bankrupt' ? 'bg-rose-500' : 'bg-sky-500'"
                :style="{ width: treasuryBarPct(company) + '%' }"
              />
            </div>
          </div>

          <!-- Stats row -->
          <div class="flex items-center justify-between text-xs text-slate-400">
            <div class="flex items-center gap-1">
              <Icon name="mdi:ship" class="text-sky-400 text-sm" />
              <span class="font-mono">{{ companyShipCount(company.id) }}</span>
              <span class="text-slate-600">ships</span>
            </div>
            <div v-if="companyWarehouseCount(company.id)" class="flex items-center gap-1">
              <Icon name="mdi:warehouse" class="text-purple-400 text-sm" />
              <span class="font-mono">{{ companyWarehouseCount(company.id) }}</span>
            </div>
            <div v-if="companyUpkeep(company.id)" class="flex items-center gap-1">
              <span class="text-rose-400 font-mono text-[11px]">({{ formatCurrency(companyUpkeep(company.id)) }}/cycle)</span>
            </div>
          </div>

          <!-- Ship status dots -->
          <div class="flex items-center justify-between mt-3 pt-3 border-t border-slate-700/50">
            <div class="flex gap-1 flex-wrap">
              <span
                v-for="n in companyDockedCount(company.id)"
                :key="'d' + n"
                class="w-2 h-2 rounded-full bg-emerald-400"
                title="Docked"
              />
              <span
                v-for="n in companySailingCount(company.id)"
                :key="'s' + n"
                class="w-2 h-2 rounded-full bg-sky-400"
                title="Sailing"
              />
            </div>
            <span class="text-[10px] text-slate-600 group-hover:text-slate-400 transition-colors flex items-center gap-1">
              <Icon name="lucide:arrow-right" class="text-[10px]" />
              Details
            </span>
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

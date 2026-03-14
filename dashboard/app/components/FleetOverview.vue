<script setup lang="ts">
const props = defineProps<{
  companyId: number
}>()

const { inventory, loading, fetchInventory, startPolling, stopPolling } = useInventory()
const { now } = useNow()

watch(
  () => props.companyId,
  (id) => {
    if (id) startPolling(id, 10000)
  },
  { immediate: true }
)

onUnmounted(() => stopPolling())

function formatCurrency(value: number): string {
  return new Intl.NumberFormat('en-US').format(value)
}

function timeUntilArrival(arrivingAt: string): string {
  const diff = new Date(arrivingAt).getTime() - now.value
  if (diff <= 0) return 'Arriving...'
  const mins = Math.floor(diff / 60000)
  const secs = Math.floor((diff % 60000) / 1000)
  if (mins > 0) return `${mins}m ${secs}s`
  return `${secs}s`
}

function estimateTravelTime(distance: number, speed: number): string {
  const mins = Math.ceil(distance / speed)
  if (mins >= 60) return `${Math.floor(mins / 60)}h ${mins % 60}m`
  return `${mins}m`
}

function arrivalProgress(arrivingAt: string, distance?: number): number {
  if (!distance || !arrivingAt) return 0
  const arriveTime = new Date(arrivingAt).getTime()
  const totalMs = distance * 60000
  const departTime = arriveTime - totalMs
  const elapsed = now.value - departTime
  return Math.min(100, Math.max(0, (elapsed / totalMs) * 100))
}

function statusIcon(status: string): string {
  switch (status) {
    case 'docked': return 'lucide:anchor'
    case 'traveling': return 'mdi:sail-boat'
    default: return 'lucide:circle'
  }
}

function statusClasses(status: string): string {
  switch (status) {
    case 'docked': return 'text-emerald-400'
    case 'traveling': return 'text-sky-400'
    default: return 'text-slate-400'
  }
}

function shipTypeColor(shipType: string): string {
  switch (shipType?.toLowerCase()) {
    case 'cog': return 'bg-slate-500/20 text-slate-300'
    case 'caravel': return 'bg-blue-500/20 text-blue-300'
    case 'galleon': return 'bg-amber-500/20 text-amber-300'
    default: return 'bg-slate-500/20 text-slate-300'
  }
}

const totalCargoValue = computed(() =>
  inventory.value?.ships.reduce((sum, s) => sum + s.cargo_total, 0) ?? 0
)

const totalWarehouseItems = computed(() =>
  inventory.value?.warehouses.reduce(
    (sum, w) => sum + w.items.reduce((s, i) => s + i.quantity, 0),
    0
  ) ?? 0
)

const totalCapacity = computed(() =>
  inventory.value?.ships.reduce((sum, s) => sum + s.capacity, 0) ?? 0
)
</script>

<template>
  <div class="bg-slate-800 rounded-lg shadow-lg border border-slate-700 p-5">
    <div class="flex items-center justify-between mb-4">
      <h3 class="text-sm font-semibold text-slate-300 flex items-center gap-2">
        <Icon name="mdi:sail-boat" class="text-sky-400" />
        Fleet & Inventory
      </h3>
      <button
        class="text-xs text-slate-500 hover:text-slate-300 transition-colors"
        @click="fetchInventory(companyId)"
      >
        <Icon name="lucide:refresh-cw" class="mr-1" />
        Refresh
      </button>
    </div>

    <div v-if="loading && !inventory" class="flex items-center justify-center py-8">
      <Icon name="mdi:loading" class="animate-spin text-2xl text-slate-500" />
    </div>

    <template v-else-if="inventory">
      <!-- Summary Stats -->
      <div class="grid grid-cols-3 lg:grid-cols-5 gap-3 mb-4">
        <div class="bg-slate-900/50 rounded-lg p-2.5">
          <div class="text-[10px] text-slate-500 uppercase tracking-wide">Ships</div>
          <div class="text-base font-bold text-slate-100 font-mono">{{ inventory.ships.length }}</div>
        </div>
        <div class="bg-slate-900/50 rounded-lg p-2.5">
          <div class="text-[10px] text-slate-500 uppercase tracking-wide">Upkeep/hr</div>
          <div class="text-base font-bold text-amber-400 font-mono">{{ formatCurrency(inventory.total_upkeep) }}</div>
        </div>
        <div class="bg-slate-900/50 rounded-lg p-2.5">
          <div class="text-[10px] text-slate-500 uppercase tracking-wide">Cargo</div>
          <div class="text-base font-bold text-sky-400 font-mono">{{ totalCargoValue }} / {{ totalCapacity }}</div>
        </div>
        <div class="bg-slate-900/50 rounded-lg p-2.5">
          <div class="text-[10px] text-slate-500 uppercase tracking-wide">Warehoused</div>
          <div class="text-base font-bold text-purple-400 font-mono">{{ totalWarehouseItems }}</div>
        </div>
        <div class="bg-slate-900/50 rounded-lg p-2.5">
          <div class="text-[10px] text-slate-500 uppercase tracking-wide">Treasury</div>
          <div class="text-base font-bold text-amber-400 font-mono">{{ formatCurrency(inventory.treasury) }}</div>
        </div>
      </div>

      <!-- Ships -->
      <div v-if="inventory.ships.length > 0" class="space-y-2 max-h-[32rem] 2xl:max-h-[40rem] overflow-y-auto scroll-stable">
        <div
          v-for="ship in inventory.ships"
          :key="ship.ship_id"
          class="bg-slate-900/50 rounded-lg border border-slate-700/50 p-3"
        >
          <!-- Row 1: Name + Type badge -->
          <div class="flex items-center justify-between mb-2">
            <div class="flex items-center gap-2">
              <Icon
                :name="statusIcon(ship.status)"
                :class="statusClasses(ship.status)"
                class="flex-shrink-0"
              />
              <span class="text-sm font-medium text-slate-200">{{ ship.ship_name }}</span>
              <span
                v-if="ship.ship_type"
                class="px-1.5 py-0.5 rounded text-[10px] font-medium"
                :class="shipTypeColor(ship.ship_type)"
              >
                {{ ship.ship_type }}
              </span>
            </div>
          </div>

          <!-- Row 2: Location / Route -->
          <div class="flex items-center gap-2 text-xs mb-2">
            <!-- Docked -->
            <template v-if="ship.status === 'docked' && ship.port_name">
              <span class="text-emerald-400 font-medium">Docked</span>
              <span class="text-slate-600">at</span>
              <span class="text-slate-300">{{ ship.port_name }}</span>
            </template>
            <!-- Traveling with route info -->
            <template v-else-if="ship.status === 'traveling'">
              <span class="text-sky-400 font-medium">Sailing</span>
              <template v-if="ship.from_port_name && ship.to_port_name">
                <span class="text-slate-400">{{ ship.from_port_name }}</span>
                <Icon name="lucide:arrow-right" class="text-[10px] text-slate-600" />
                <span class="text-slate-300">{{ ship.to_port_name }}</span>
              </template>
            </template>
            <!-- Other status -->
            <template v-else>
              <span class="text-slate-400 capitalize">{{ ship.status }}</span>
            </template>
          </div>

          <!-- ETA countdown + progress bar -->
          <div v-if="ship.status === 'traveling'" class="mb-2">
            <div class="flex items-center justify-between mb-1">
              <span class="text-xs text-slate-500">
                {{ ship.arriving_at ? 'Arrival in' : (ship.distance && ship.speed ? 'Est. travel' : 'In transit') }}
              </span>
              <span v-if="ship.arriving_at" class="text-sm font-mono font-bold text-emerald-400">
                {{ timeUntilArrival(ship.arriving_at) }}
              </span>
              <span v-else-if="ship.distance && ship.speed" class="text-sm font-mono font-bold text-slate-400">
                ~{{ estimateTravelTime(ship.distance, ship.speed) }}
              </span>
            </div>
            <div v-if="ship.arriving_at" class="h-1.5 rounded-full bg-slate-700 overflow-hidden">
              <div
                class="h-full rounded-full bg-emerald-500 transition-all duration-1000"
                :style="{ width: `${arrivalProgress(ship.arriving_at, ship.distance)}%` }"
              />
            </div>
          </div>

          <!-- Row 3: Ship stats (always visible, with labels) -->
          <div class="grid gap-2 mb-2" :class="ship.passenger_cap > 0 ? 'grid-cols-5' : 'grid-cols-4'">
            <div class="bg-slate-800/50 rounded px-2 py-1">
              <div class="text-[10px] text-slate-500 uppercase">Capacity</div>
              <div class="text-xs font-mono text-slate-300">{{ ship.capacity }}</div>
            </div>
            <div class="bg-slate-800/50 rounded px-2 py-1">
              <div class="text-[10px] text-slate-500 uppercase">Speed</div>
              <div class="text-xs font-mono text-slate-300">{{ ship.speed }}</div>
            </div>
            <div class="bg-slate-800/50 rounded px-2 py-1">
              <div class="text-[10px] text-slate-500 uppercase">Upkeep</div>
              <div class="text-xs font-mono text-amber-400">{{ formatCurrency(ship.upkeep) }}/hr</div>
            </div>
            <div class="bg-slate-800/50 rounded px-2 py-1">
              <div class="text-[10px] text-slate-500 uppercase">Cargo</div>
              <div class="text-xs font-mono" :class="ship.cargo_total > 0 ? 'text-amber-400' : 'text-slate-500'">
                {{ ship.cargo_total }} / {{ ship.capacity }}
              </div>
            </div>
            <div v-if="ship.passenger_cap > 0" class="bg-slate-800/50 rounded px-2 py-1">
              <div class="text-[10px] text-slate-500 uppercase">Passengers</div>
              <div class="text-xs font-mono" :class="ship.passenger_count > 0 ? 'text-cyan-400' : 'text-slate-500'">
                {{ ship.passenger_count }} / {{ ship.passenger_cap }}
              </div>
            </div>
          </div>

          <!-- Capacity fill bar -->
          <div v-if="ship.capacity > 0 || ship.passenger_cap > 0" class="mb-2">
            <div class="flex items-center gap-1.5 mb-1">
              <span v-if="ship.capacity > 0" class="flex items-center gap-1 text-[10px]">
                <span class="inline-block w-2 h-2 rounded-sm bg-amber-500" />
                <span class="text-slate-400">Cargo {{ ship.cargo_total }}/{{ ship.capacity }}</span>
              </span>
              <span v-if="ship.passenger_cap > 0" class="flex items-center gap-1 text-[10px]">
                <span class="inline-block w-2 h-2 rounded-sm bg-cyan-500" />
                <span class="text-slate-400">Passengers {{ ship.passenger_count }}/{{ ship.passenger_cap }}</span>
              </span>
            </div>
            <div class="h-1.5 rounded-full bg-slate-700 overflow-hidden flex">
              <div
                v-if="ship.capacity > 0"
                class="h-full bg-amber-500 transition-all"
                :style="{ width: `${(ship.cargo_total / (ship.capacity + ship.passenger_cap)) * 100}%` }"
              />
              <div
                v-if="ship.passenger_cap > 0"
                class="h-full bg-cyan-500 transition-all"
                :style="{ width: `${(ship.passenger_count / (ship.capacity + ship.passenger_cap)) * 100}%` }"
              />
            </div>
          </div>

          <!-- Cargo Details -->
          <div v-if="ship.cargo.length > 0" class="flex flex-wrap gap-1.5">
            <span
              v-for="item in ship.cargo"
              :key="item.good_id"
              class="inline-flex items-center gap-1 px-2 py-0.5 rounded-full bg-slate-800 text-[10px] text-slate-400 border border-slate-700/50"
            >
              {{ item.good_name }}
              <span class="font-mono text-slate-300">x{{ item.quantity }}</span>
            </span>
          </div>
        </div>
      </div>

      <div v-else class="text-center text-slate-600 text-sm py-4">
        No ships owned yet
      </div>

      <!-- Warehouses -->
      <div v-if="inventory.warehouses.length > 0" class="mt-4">
        <h4 class="text-xs font-semibold text-slate-400 uppercase tracking-wide mb-2">Warehouses</h4>
        <div class="space-y-1.5 max-h-48 2xl:max-h-64 overflow-y-auto scroll-stable">
          <div
            v-for="wh in inventory.warehouses"
            :key="wh.warehouse_id"
            class="bg-slate-900/50 rounded-lg border border-slate-700/50 p-2.5"
          >
            <div class="flex items-center justify-between mb-1">
              <div class="flex items-center gap-2">
                <Icon name="lucide:warehouse" class="text-purple-400 text-xs" />
                <span class="text-sm text-slate-300">{{ wh.port_name }}</span>
                <span class="text-[10px] text-slate-500">Lv.{{ wh.level }}</span>
              </div>
              <span class="text-xs text-slate-500 font-mono">
                {{ wh.items.reduce((s: number, i: { quantity: number }) => s + i.quantity, 0) }} / {{ wh.capacity }}
              </span>
            </div>
            <!-- Capacity bar -->
            <div class="h-1 rounded-full bg-slate-700 overflow-hidden mb-1.5">
              <div
                class="h-full rounded-full bg-purple-500 transition-all"
                :style="{ width: `${Math.min(100, (wh.items.reduce((s: number, i: { quantity: number }) => s + i.quantity, 0) / wh.capacity) * 100)}%` }"
              />
            </div>
            <div v-if="wh.items.length > 0" class="flex flex-wrap gap-1">
              <span
                v-for="item in wh.items"
                :key="item.good_id"
                class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded bg-slate-800 text-[10px] text-slate-400"
              >
                {{ item.good_name }} <span class="font-mono text-slate-300">x{{ item.quantity }}</span>
              </span>
            </div>
            <div v-else class="text-[10px] text-slate-600 italic">Empty</div>
          </div>
        </div>
      </div>
    </template>

    <div v-else class="text-center text-slate-600 text-sm py-8">
      No inventory data available
    </div>
  </div>
</template>

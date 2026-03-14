<script setup lang="ts">
import type { ShipDetail, WarehouseDetail } from '~/types'

const props = defineProps<{
  companyId: number
}>()

const { inventory, loading, fetchInventory, startPolling, stopPolling } = useInventory()

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
  const diff = new Date(arrivingAt).getTime() - Date.now()
  if (diff <= 0) return 'Arriving...'
  const mins = Math.floor(diff / 60000)
  const secs = Math.floor((diff % 60000) / 1000)
  if (mins > 0) return `${mins}m ${secs}s`
  return `${secs}s`
}

function statusIcon(status: string): string {
  switch (status) {
    case 'docked': return 'lucide:anchor'
    case 'sailing': return 'mdi:sail-boat'
    default: return 'lucide:circle'
  }
}

function statusClasses(status: string): string {
  switch (status) {
    case 'docked': return 'text-emerald-400'
    case 'sailing': return 'text-sky-400'
    default: return 'text-slate-400'
  }
}

const dockedShips = computed(() =>
  inventory.value?.ships.filter(s => s.status === 'docked') ?? []
)

const sailingShips = computed(() =>
  inventory.value?.ships.filter(s => s.status !== 'docked') ?? []
)

const totalCargoValue = computed(() =>
  inventory.value?.ships.reduce((sum, s) => sum + s.cargo_total, 0) ?? 0
)

const totalWarehouseItems = computed(() =>
  inventory.value?.warehouses.reduce(
    (sum, w) => sum + w.items.reduce((s, i) => s + i.quantity, 0),
    0
  ) ?? 0
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
      <div class="grid grid-cols-2 lg:grid-cols-4 gap-3 mb-4">
        <div class="bg-slate-900/50 rounded-lg p-2.5">
          <div class="text-[10px] text-slate-500 uppercase tracking-wide">Ships</div>
          <div class="text-base font-bold text-slate-100 font-mono">{{ inventory.ships.length }}</div>
        </div>
        <div class="bg-slate-900/50 rounded-lg p-2.5">
          <div class="text-[10px] text-slate-500 uppercase tracking-wide">Upkeep/hr</div>
          <div class="text-base font-bold text-amber-400 font-mono">{{ formatCurrency(inventory.total_upkeep) }}</div>
        </div>
        <div class="bg-slate-900/50 rounded-lg p-2.5">
          <div class="text-[10px] text-slate-500 uppercase tracking-wide">Cargo in Transit</div>
          <div class="text-base font-bold text-sky-400 font-mono">{{ totalCargoValue }}</div>
        </div>
        <div class="bg-slate-900/50 rounded-lg p-2.5">
          <div class="text-[10px] text-slate-500 uppercase tracking-wide">Warehoused</div>
          <div class="text-base font-bold text-purple-400 font-mono">{{ totalWarehouseItems }}</div>
        </div>
      </div>

      <!-- Ships -->
      <div v-if="inventory.ships.length > 0" class="space-y-1.5 max-h-72 overflow-y-auto">
        <div
          v-for="ship in inventory.ships"
          :key="ship.ship_id"
          class="bg-slate-900/50 rounded-lg border border-slate-700/50 p-3"
        >
          <div class="flex items-center justify-between">
            <div class="flex items-center gap-2 min-w-0">
              <Icon
                :name="statusIcon(ship.status)"
                :class="statusClasses(ship.status)"
                class="flex-shrink-0"
              />
              <div class="min-w-0">
                <span class="text-sm font-medium text-slate-200 truncate block">{{ ship.ship_name }}</span>
                <div class="flex items-center gap-2 text-xs text-slate-500">
                  <span class="capitalize">{{ ship.status }}</span>
                  <template v-if="ship.port_name">
                    <span class="text-slate-600">at</span>
                    <span class="text-slate-400">{{ ship.port_name }}</span>
                  </template>
                  <template v-if="ship.status === 'sailing' && ship.arriving_at">
                    <span class="text-slate-600">ETA</span>
                    <span class="text-sky-400 font-mono">{{ timeUntilArrival(ship.arriving_at) }}</span>
                  </template>
                </div>
              </div>
            </div>

            <div class="flex items-center gap-3 flex-shrink-0">
              <div v-if="ship.cargo.length > 0" class="text-right">
                <div class="text-[10px] text-slate-500">Cargo</div>
                <div class="text-xs text-slate-300 font-mono">{{ ship.cargo_total }} units</div>
              </div>
              <div v-else class="text-xs text-slate-600 italic">Empty</div>
            </div>
          </div>

          <!-- Cargo Details -->
          <div v-if="ship.cargo.length > 0" class="mt-2 flex flex-wrap gap-1.5">
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
        <div class="space-y-1.5 max-h-48 overflow-y-auto">
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

<script setup lang="ts">
const { world, loading, fetchWorld } = useWorld()

onMounted(() => fetchWorld())

const activeTab = ref<'ports' | 'goods' | 'routes' | 'ships'>('ports')

function formatCurrency(value: number): string {
  return new Intl.NumberFormat('en-US').format(value)
}
</script>

<template>
  <div class="bg-slate-800 rounded-lg shadow-lg border border-slate-700 p-5">
    <div class="flex items-center justify-between mb-4">
      <h3 class="text-sm font-semibold text-slate-300 flex items-center gap-2">
        <Icon name="lucide:globe" class="text-teal-400" />
        World Data
      </h3>
      <button
        class="text-xs text-slate-500 hover:text-slate-300 transition-colors"
        @click="fetchWorld"
      >
        <Icon name="lucide:refresh-cw" class="mr-1" />
        Refresh
      </button>
    </div>

    <div v-if="loading" class="flex items-center justify-center py-8">
      <Icon name="mdi:loading" class="animate-spin text-2xl text-slate-500" />
    </div>

    <template v-else-if="world">
      <!-- Tabs -->
      <div class="flex gap-1 mb-3 border-b border-slate-700 pb-2">
        <button
          v-for="tab in (['ports', 'goods', 'routes', 'ships'] as const)"
          :key="tab"
          class="px-3 py-1 rounded text-xs font-medium transition-colors"
          :class="activeTab === tab
            ? 'bg-slate-700 text-slate-200'
            : 'text-slate-500 hover:text-slate-300'"
          @click="activeTab = tab"
        >
          {{ tab === 'ships' ? 'Ship Types' : tab.charAt(0).toUpperCase() + tab.slice(1) }}
          <span class="ml-1 text-slate-500">
            {{ tab === 'ports' ? world.ports.length
              : tab === 'goods' ? world.goods.length
              : tab === 'routes' ? world.routes.length
              : world.ship_types.length }}
          </span>
        </button>
      </div>

      <!-- Ports Tab -->
      <div v-if="activeTab === 'ports'" class="max-h-96 2xl:max-h-[36rem] overflow-y-auto scroll-stable">
        <table class="w-full text-xs">
          <thead class="sticky top-0 bg-slate-800">
            <tr class="text-slate-500 border-b border-slate-700">
              <th class="text-left py-1.5 pr-2">Port</th>
              <th class="text-left py-1.5 pr-2">Code</th>
              <th class="text-center py-1.5 pr-2">Hub</th>
              <th class="text-right py-1.5 pr-2">Tax</th>
              <th class="text-center py-1.5">Shipyard</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="port in world.ports"
              :key="port.id"
              class="border-b border-slate-700/20"
            >
              <td class="py-1.5 pr-2 text-slate-300">{{ port.name }}</td>
              <td class="py-1.5 pr-2 text-slate-500 font-mono">{{ port.code }}</td>
              <td class="py-1.5 pr-2 text-center">
                <Icon v-if="port.is_hub" name="lucide:star" class="text-amber-400 text-xs" />
              </td>
              <td class="py-1.5 pr-2 text-right font-mono text-slate-400">{{ port.tax_rate.toFixed(1) }}%</td>
              <td class="py-1.5 text-center">
                <Icon v-if="port.has_shipyard" name="lucide:hammer" class="text-sky-400 text-xs" />
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- Goods Tab -->
      <div v-if="activeTab === 'goods'" class="max-h-96 2xl:max-h-[36rem] overflow-y-auto scroll-stable">
        <table class="w-full text-xs">
          <thead class="sticky top-0 bg-slate-800">
            <tr class="text-slate-500 border-b border-slate-700">
              <th class="text-left py-1.5 pr-2">Good</th>
              <th class="text-left py-1.5 pr-2">Category</th>
              <th class="text-left py-1.5">Description</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="good in world.goods"
              :key="good.id"
              class="border-b border-slate-700/20"
            >
              <td class="py-1.5 pr-2 text-slate-300 font-medium">{{ good.name }}</td>
              <td class="py-1.5 pr-2">
                <span class="px-1.5 py-0.5 rounded bg-slate-700 text-slate-400 text-[10px]">
                  {{ good.category }}
                </span>
              </td>
              <td class="py-1.5 text-slate-500 truncate max-w-xs">{{ good.description }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- Routes Tab -->
      <div v-if="activeTab === 'routes'" class="max-h-96 2xl:max-h-[36rem] overflow-y-auto scroll-stable">
        <table class="w-full text-xs">
          <thead class="sticky top-0 bg-slate-800">
            <tr class="text-slate-500 border-b border-slate-700">
              <th class="text-left py-1.5 pr-2">From</th>
              <th class="text-center py-1.5 pr-2"></th>
              <th class="text-left py-1.5 pr-2">To</th>
              <th class="text-right py-1.5">Distance</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="route in world.routes"
              :key="route.id"
              class="border-b border-slate-700/20"
            >
              <td class="py-1.5 pr-2 text-slate-300">{{ route.from_port_name }}</td>
              <td class="py-1.5 pr-2 text-center">
                <Icon name="lucide:arrow-right" class="text-slate-600 text-xs" />
              </td>
              <td class="py-1.5 pr-2 text-slate-300">{{ route.to_port_name }}</td>
              <td class="py-1.5 text-right font-mono text-slate-400">{{ route.distance.toFixed(1) }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- Ship Types Tab -->
      <div v-if="activeTab === 'ships'" class="max-h-96 2xl:max-h-[36rem] overflow-y-auto scroll-stable">
        <div class="grid grid-cols-1 md:grid-cols-3 gap-3">
          <div
            v-for="st in world.ship_types"
            :key="st.id"
            class="bg-slate-900/50 rounded-lg border border-slate-700/50 p-3"
          >
            <div class="text-sm font-semibold text-slate-200 mb-2">{{ st.name }}</div>
            <div class="grid grid-cols-2 gap-2 text-xs">
              <div>
                <span class="text-slate-500">Capacity</span>
                <div class="font-mono text-slate-300">{{ st.capacity }}</div>
              </div>
              <div>
                <span class="text-slate-500">Speed</span>
                <div class="font-mono text-slate-300">{{ st.speed }}</div>
              </div>
              <div>
                <span class="text-slate-500">Upkeep</span>
                <div class="font-mono text-amber-400">{{ formatCurrency(st.upkeep) }}/cycle</div>
              </div>
              <div>
                <span class="text-slate-500">Base Price</span>
                <div class="font-mono text-slate-300">{{ formatCurrency(st.base_price) }}</div>
              </div>
              <div v-if="st.passenger_cap > 0" class="col-span-2">
                <span class="text-slate-500">Passengers</span>
                <div class="font-mono text-purple-400">{{ st.passenger_cap }}</div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </template>

    <div v-else class="text-center text-slate-600 text-sm py-8">
      World data not available
    </div>
  </div>
</template>

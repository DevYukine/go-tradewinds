<script setup lang="ts">
import type { ShipDetail } from '~/types'

const props = defineProps<{
  companyId: number
}>()

const companyIdRef = computed(() => props.companyId)
const { world, fetchWorld } = useWorld()
const { shipsByPort, shipsInTransit } = useShipPositions(companyIdRef)

onMounted(() => {
  if (!world.value) fetchWorld()
})

// Build set of route IDs that have ships in transit
const activeRouteIds = computed(() => {
  const ids = new Set<string>()
  for (const ship of shipsInTransit.value) {
    if (ship.route_id) ids.add(ship.route_id)
  }
  return ids
})

// Build set of port IDs that have docked ships
const activePortIds = computed(() => new Set(shipsByPort.value.keys()))

const graphData = computed(() => {
  if (!world.value) return { nodes: [], links: [] }

  const nodes = world.value.ports.map(port => {
    const dockedShips = shipsByPort.value.get(port.id) || []
    return {
      id: port.id,
      portName: port.name,
      code: port.code,
      isHub: port.is_hub,
      hasShipyard: port.has_shipyard,
      shipCount: dockedShips.length,
      ships: dockedShips,
    }
  })

  const links = world.value.routes.map(route => ({
    source: route.from_port_id,
    target: route.to_port_id,
    routeId: route.id,
    distance: route.distance,
    label: `${route.distance}`,
    active: activeRouteIds.value.has(route.id),
  }))

  return { nodes, links }
})

function nodeLabel(node: any) {
  if (node.shipCount > 0) return `${node.portName} (${node.shipCount})`
  return node.portName
}

function nodeFill(node: any) {
  if (node.shipCount > 0) return '#14b8a6'
  if (node.isHub) return '#3b82f6'
  if (node.hasShipyard) return '#f59e0b'
  return '#64748b'
}

function nodeStroke(node: any) {
  if (node.shipCount > 0) return '#0d9488'
  return '#475569'
}

function linkStroke(link: any) {
  if (link.active) return '#f59e0b'
  return '#334155'
}

function linkWidth(link: any) {
  if (link.active) return 3
  return 1.5
}

function tooltipTitle(node: any) {
  return node.portName
}

function tooltipContent(node: any) {
  const lines: string[] = []
  if (node.isHub) lines.push('Hub Port')
  if (node.hasShipyard) lines.push('Has Shipyard')
  if (node.shipCount > 0) {
    lines.push(`${node.shipCount} ship${node.shipCount > 1 ? 's' : ''} docked`)
    for (const ship of node.ships as ShipDetail[]) {
      const cargo = ship.cargo_total > 0 ? ` (${ship.cargo_total} cargo)` : ''
      lines.push(`  ${ship.ship_name}${cargo}`)
    }
  }
  return lines.join('<br>')
}

const legendItems = [
  { name: 'Ships Docked', color: '#14b8a6' },
  { name: 'Hub Port', color: '#3b82f6' },
  { name: 'Shipyard', color: '#f59e0b' },
  { name: 'Port', color: '#64748b' },
]
</script>

<template>
  <div class="bg-slate-800 rounded-lg shadow-lg border border-slate-700 p-5">
    <div class="flex items-center justify-between mb-4">
      <h3 class="text-sm font-semibold text-slate-300 flex items-center gap-2">
        <Icon name="lucide:globe" class="text-blue-400" />
        World Map
      </h3>
      <div class="flex items-center gap-3">
        <span v-if="shipsInTransit.length > 0" class="text-xs text-amber-400">
          {{ shipsInTransit.length }} ship{{ shipsInTransit.length > 1 ? 's' : '' }} in transit
        </span>
      </div>
    </div>

    <div v-if="!world" class="h-[400px] flex items-center justify-center">
      <Icon name="mdi:loading" class="animate-spin text-2xl text-slate-500" />
    </div>
    <div v-else-if="graphData.nodes.length === 0" class="h-[400px] flex items-center justify-center text-slate-600 text-sm">
      No world data available
    </div>
    <div v-else class="world-map">
      <DagreGraph
        :data="graphData"
        :height="400"
        :dagre-layout-settings="{ rankdir: 'LR', nodesep: 80, ranksep: 120 }"
        :node-size="40"
        :node-label="nodeLabel"
        node-shape="circle"
        :node-fill="nodeFill"
        :node-stroke="nodeStroke"
        :node-stroke-width="2"
        link-arrow="end"
        :link-stroke="linkStroke"
        :link-width="linkWidth"
        :zoom-enabled="true"
        :zoom-scale-extent="[0.5, 3]"
        :legend-items="legendItems"
        legend-position="top-right"
        :tooltip-title-formatter="tooltipTitle"
        :tooltip-content-formatter="tooltipContent"
        :duration="600"
      />
    </div>
  </div>
</template>

<style scoped>
.world-map :deep(svg) {
  overflow: visible;
}
.world-map :deep(.unovis-dagre-node text) {
  fill: #cbd5e1;
  font-size: 10px;
}
.world-map :deep(.unovis-dagre-link text) {
  fill: #64748b;
  font-size: 9px;
}
</style>

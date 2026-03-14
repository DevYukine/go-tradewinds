<script setup lang="ts">
import L from 'leaflet'
import 'leaflet/dist/leaflet.css'
import type { MapShip, PortInfo, RouteInfo, WorldData } from '~/types'

const config = useRuntimeConfig()
const apiBase = config.public.apiBase

const mapContainer = ref<HTMLDivElement | null>(null)
let map: L.Map | null = null
let pollTimer: ReturnType<typeof setInterval> | null = null

const portLayer = L.layerGroup()
const routeLayer = L.layerGroup()
const shipLayer = L.layerGroup()

const STRATEGY_COLORS: Record<string, string> = {
  arbitrage: '#10b981',
  bulk_hauler: '#3b82f6',
  market_maker: '#f59e0b',
}

const ports = ref<PortInfo[]>([])
const routes = ref<RouteInfo[]>([])
const ships = ref<MapShip[]>([])
const loading = ref(true)

function portCoords(port: PortInfo): L.LatLngExpression | null {
  if (port.latitude != null && port.longitude != null) {
    return [port.latitude, port.longitude]
  }
  return null
}

function buildPortLookup(): Map<string, PortInfo> {
  const lookup = new Map<string, PortInfo>()
  for (const port of ports.value) {
    lookup.set(port.id, port)
  }
  return lookup
}

function drawPorts() {
  portLayer.clearLayers()
  for (const port of ports.value) {
    const coords = portCoords(port)
    if (!coords) continue

    const isHub = port.is_hub
    const radius = isHub ? 8 : 6
    const color = isHub ? '#f59e0b' : '#14b8a6'

    const marker = L.circleMarker(coords, {
      radius,
      fillColor: color,
      fillOpacity: 0.9,
      color: port.has_shipyard ? '#e2e8f0' : color,
      weight: port.has_shipyard ? 2.5 : 1.5,
    })

    const hubBadge = isHub ? '<span class="text-amber-400 font-semibold">Hub Port</span>' : 'Regular Port'
    const shipyardBadge = port.has_shipyard ? ' &middot; Shipyard' : ''

    marker.bindPopup(
      `<div class="text-sm">
        <div class="font-bold mb-1">${port.name} (${port.code})</div>
        <div>${hubBadge}${shipyardBadge}</div>
        <div>Tax: ${port.tax_rate.toFixed(1)}%</div>
      </div>`,
    )

    portLayer.addLayer(marker)

    // Port name label
    const label = L.marker(coords, {
      icon: L.divIcon({
        className: 'port-label',
        html: `<span>${port.name}</span>`,
        iconSize: [0, 0],
        iconAnchor: [-10, 4],
      }),
      interactive: false,
    })
    portLayer.addLayer(label)
  }
}

function drawRoutes() {
  routeLayer.clearLayers()
  const lookup = buildPortLookup()

  for (const route of routes.value) {
    const fromPort = lookup.get(route.from_port_id)
    const toPort = lookup.get(route.to_port_id)
    if (!fromPort || !toPort) continue

    const fromCoords = portCoords(fromPort)
    const toCoords = portCoords(toPort)
    if (!fromCoords || !toCoords) continue

    const line = L.polyline([fromCoords, toCoords], {
      color: '#475569',
      weight: 1,
      opacity: 0.4,
      dashArray: '6 4',
    })

    routeLayer.addLayer(line)
  }
}

function buildRouteLookup(): Map<string, RouteInfo> {
  const lookup = new Map<string, RouteInfo>()
  for (const route of routes.value) {
    lookup.set(`${route.from_port_id}:${route.to_port_id}`, route)
  }
  return lookup
}

// Stable offsets for docked ships so they don't jitter on every redraw.
const dockedOffsets = new Map<string, [number, number]>()

function getDockedOffset(shipId: string): [number, number] {
  if (!dockedOffsets.has(shipId)) {
    dockedOffsets.set(shipId, [(Math.random() - 0.5) * 0.12, (Math.random() - 0.5) * 0.12])
  }
  return dockedOffsets.get(shipId)!
}

function drawShips() {
  shipLayer.clearLayers()
  const portLookup = buildPortLookup()
  const routeLookup = buildRouteLookup()
  const now = Date.now()

  for (const ship of ships.value) {
    let coords: L.LatLngExpression | null = null
    const strategyColor = STRATEGY_COLORS[ship.strategy] ?? '#94a3b8'

    if (ship.status === 'docked' && ship.port_id) {
      const port = portLookup.get(ship.port_id)
      if (!port) continue
      const portPos = portCoords(port)
      if (!portPos) continue
      const offset = getDockedOffset(ship.ship_id)
      const lat = (portPos as [number, number])[0] + offset[0]
      const lng = (portPos as [number, number])[1] + offset[1]
      coords = [lat, lng]
    } else if (ship.from_port_id && ship.to_port_id) {
      const fromPort = portLookup.get(ship.from_port_id)
      const toPort = portLookup.get(ship.to_port_id)
      if (!fromPort || !toPort) continue

      const fromPos = portCoords(fromPort)
      const toPos = portCoords(toPort)
      if (!fromPos || !toPos) continue

      let progress = 0.5 // default mid-route
      if (ship.arriving_at) {
        // Use route distance to estimate travel time for better interpolation
        const route = routeLookup.get(`${ship.from_port_id}:${ship.to_port_id}`)
        const travelMinutes = route ? route.distance : 60
        const travelMs = travelMinutes * 60 * 1000
        const arrivalTime = new Date(ship.arriving_at).getTime()
        const departureTime = arrivalTime - travelMs
        progress = Math.max(0, Math.min(1, (now - departureTime) / travelMs))
      }

      const fromArr = fromPos as [number, number]
      const toArr = toPos as [number, number]
      const lat = fromArr[0] + (toArr[0] - fromArr[0]) * progress
      const lng = fromArr[1] + (toArr[1] - fromArr[1]) * progress
      coords = [lat, lng]
    }

    if (!coords) continue

    const marker = L.circleMarker(coords, {
      radius: ship.status === 'docked' ? 3 : 5,
      fillColor: strategyColor,
      fillOpacity: 0.9,
      color: ship.status === 'traveling' ? '#ffffff' : strategyColor,
      weight: ship.status === 'traveling' ? 1.5 : 1,
    })

    const cargoText = `${ship.cargo_total} / ${ship.capacity}`
    marker.bindPopup(
      `<div class="text-sm">
        <div class="font-bold mb-1">${ship.ship_name}</div>
        <div>${ship.company_name}</div>
        <div>Status: ${ship.status}</div>
        <div>Strategy: ${ship.strategy}</div>
        <div>Cargo: ${cargoText}</div>
      </div>`,
    )

    shipLayer.addLayer(marker)
  }
}

async function fetchWorldData() {
  try {
    const data = await $fetch<WorldData>(`${apiBase}/api/world`)
    ports.value = data.ports
    routes.value = data.routes
    drawPorts()
    drawRoutes()
  } catch (e) {
    console.error('WorldMap: failed to fetch world data:', e)
  } finally {
    loading.value = false
  }
}

async function fetchShips() {
  try {
    const data = await $fetch<MapShip[]>(`${apiBase}/api/ships`)
    ships.value = data
    drawShips()
  } catch (e) {
    console.error('WorldMap: failed to fetch ships:', e)
  }
}

onMounted(() => {
  if (!mapContainer.value) return

  map = L.map(mapContainer.value, {
    center: [52.5, 2.0],
    zoom: 5,
    zoomControl: true,
  })

  L.tileLayer('https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png', {
    attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OSM</a> &copy; <a href="https://carto.com/">CARTO</a>',
    subdomains: 'abcd',
    maxZoom: 19,
  }).addTo(map)

  routeLayer.addTo(map)
  portLayer.addTo(map)
  shipLayer.addTo(map)

  fetchWorldData()
  fetchShips()

  pollTimer = setInterval(fetchShips, 10_000)
})

onUnmounted(() => {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
  if (map) {
    map.remove()
    map = null
  }
})
</script>

<template>
  <div class="bg-slate-800 rounded-lg shadow-lg border border-slate-700 p-5">
    <div class="flex items-center justify-between mb-4">
      <h3 class="text-sm font-semibold text-slate-300 flex items-center gap-2">
        <Icon name="lucide:map" class="text-teal-400" />
        World Map
      </h3>
    </div>

    <div v-if="loading" class="h-[500px] flex items-center justify-center">
      <Icon name="mdi:loading" class="animate-spin text-2xl text-slate-500" />
    </div>

    <div class="relative">
      <div ref="mapContainer" class="h-[500px] rounded-lg overflow-hidden" />

      <!-- Legend -->
      <div class="absolute bottom-4 right-4 z-[1000] bg-slate-900/90 border border-slate-700 rounded-lg p-3 text-xs">
        <div class="font-semibold text-slate-300 mb-2">Legend</div>

        <div class="space-y-1.5 mb-2">
          <div class="text-slate-500 font-medium">Ports</div>
          <div class="flex items-center gap-2">
            <span class="inline-block w-3 h-3 rounded-full bg-amber-500" />
            <span class="text-slate-400">Hub Port</span>
          </div>
          <div class="flex items-center gap-2">
            <span class="inline-block w-3 h-3 rounded-full bg-teal-500" />
            <span class="text-slate-400">Regular Port</span>
          </div>
          <div class="flex items-center gap-2">
            <span class="inline-block w-2.5 h-2.5 rounded-full border-2 border-slate-200 bg-transparent" />
            <span class="text-slate-400">Shipyard</span>
          </div>
        </div>

        <div class="space-y-1.5">
          <div class="text-slate-500 font-medium">Ships</div>
          <div class="flex items-center gap-2">
            <span class="inline-block w-2.5 h-2.5 rounded-full bg-emerald-500" />
            <span class="text-slate-400">Arbitrage</span>
          </div>
          <div class="flex items-center gap-2">
            <span class="inline-block w-2.5 h-2.5 rounded-full bg-blue-500" />
            <span class="text-slate-400">Bulk Hauler</span>
          </div>
          <div class="flex items-center gap-2">
            <span class="inline-block w-2.5 h-2.5 rounded-full bg-amber-500" />
            <span class="text-slate-400">Market Maker</span>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
/* Ensure Leaflet popups are readable on the dark map */
:deep(.leaflet-popup-content-wrapper) {
  background-color: #1e293b;
  color: #e2e8f0;
  border-radius: 0.5rem;
  border: 1px solid #334155;
}

:deep(.leaflet-popup-tip) {
  background-color: #1e293b;
}

:deep(.leaflet-popup-close-button) {
  color: #94a3b8;
}

:deep(.leaflet-control-zoom a) {
  background-color: #1e293b !important;
  color: #e2e8f0 !important;
  border-color: #334155 !important;
}

:deep(.leaflet-control-attribution) {
  background-color: rgba(15, 23, 42, 0.7) !important;
  color: #64748b !important;
}

:deep(.leaflet-control-attribution a) {
  color: #94a3b8 !important;
}

.port-label span {
  color: #cbd5e1;
  font-size: 11px;
  font-weight: 600;
  text-shadow: 0 1px 3px rgba(0, 0, 0, 0.8), 0 0 6px rgba(0, 0, 0, 0.6);
  white-space: nowrap;
}
</style>

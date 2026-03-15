<script setup lang="ts">
import L from 'leaflet'
import 'leaflet/dist/leaflet.css'
import type { MapShip, PortInfo, RouteInfo, WorldData } from '~/types'
import { computeSeaRoute, interpolateAlongPath, getTrailPath, type SeaPoint } from '~/utils/seaRouting'

const config = useRuntimeConfig()
const apiBase = config.public.apiBase

const mapContainer = ref<HTMLDivElement | null>(null)
let map: L.Map | null = null
let pollTimer: ReturnType<typeof setInterval> | null = null
let worldPollTimer: ReturnType<typeof setInterval> | null = null
let animTimer: ReturnType<typeof setInterval> | null = null

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

    const fromCoords = portCoords(fromPort) as [number, number] | null
    const toCoords = portCoords(toPort) as [number, number] | null
    if (!fromCoords || !toCoords) continue

    let seaPath: SeaPoint[]
    try {
      seaPath = computeSeaRoute(fromCoords, toCoords)
    } catch {
      seaPath = [fromCoords, toCoords]
    }

    const line = L.polyline(seaPath, {
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

function computeShipProgress(ship: MapShip, routeLookup: Map<string, RouteInfo>, now: number): number {
  if (!ship.arriving_at) return 0.5
  const route = routeLookup.get(`${ship.from_port_id}:${ship.to_port_id}`)
  const travelMinutes = route ? route.distance : 60
  const travelMs = travelMinutes * 60 * 1000
  const arrivalTime = new Date(ship.arriving_at).getTime()
  const departureTime = arrivalTime - travelMs
  return Math.max(0, Math.min(1, (now - departureTime) / travelMs))
}

function formatTimeRemaining(ms: number): string {
  if (ms <= 0) return 'Arriving'
  const totalSecs = Math.ceil(ms / 1000)
  const mins = Math.floor(totalSecs / 60)
  const secs = totalSecs % 60
  if (mins > 0) return `${mins}m ${secs}s`
  return `${secs}s`
}

// Track Leaflet objects for traveling ships so we can animate them per-second.
const travelingShipMarkers = new Map<string, {
  marker: L.CircleMarker
  label: L.Marker
  trail: L.Polyline
  path: SeaPoint[]
  ship: MapShip
}>()

function drawShips() {
  shipLayer.clearLayers()
  travelingShipMarkers.clear()
  const portLookup = buildPortLookup()
  const routeLookup = buildRouteLookup()
  const now = Date.now()

  for (const ship of ships.value) {
    const strategyColor = STRATEGY_COLORS[ship.strategy] ?? '#94a3b8'

    if (ship.status === 'docked' && ship.port_id) {
      const port = portLookup.get(ship.port_id)
      if (!port) continue
      const portPos = portCoords(port)
      if (!portPos) continue
      const offset = getDockedOffset(ship.ship_id)
      const lat = (portPos as [number, number])[0] + offset[0]
      const lng = (portPos as [number, number])[1] + offset[1]

      const marker = L.circleMarker([lat, lng], {
        radius: 3,
        fillColor: strategyColor,
        fillOpacity: 0.9,
        color: strategyColor,
        weight: 1,
      })

      marker.bindPopup(buildShipPopup(ship), { maxWidth: 280 })
      shipLayer.addLayer(marker)
    } else if (ship.from_port_id && ship.to_port_id) {
      const fromPort = portLookup.get(ship.from_port_id)
      const toPort = portLookup.get(ship.to_port_id)
      if (!fromPort || !toPort) continue

      const fromPos = portCoords(fromPort) as [number, number] | null
      const toPos = portCoords(toPort) as [number, number] | null
      if (!fromPos || !toPos) continue

      let seaPath: SeaPoint[]
      try {
        seaPath = computeSeaRoute(fromPos, toPos)
      } catch {
        seaPath = [fromPos, toPos]
      }
      const progress = computeShipProgress(ship, routeLookup, now)
      const pos = interpolateAlongPath(seaPath, progress)
      const trailPath = getTrailPath(seaPath, progress)

      // Trail line from origin to current position along sea route
      const trail = L.polyline(trailPath, {
        color: strategyColor,
        weight: 2,
        opacity: 0.5,
      })
      shipLayer.addLayer(trail)

      // Ship marker
      const marker = L.circleMarker(pos, {
        radius: 6,
        fillColor: strategyColor,
        fillOpacity: 1,
        color: '#ffffff',
        weight: 2,
      })

      marker.bindPopup(buildShipPopup(ship), { maxWidth: 280 })
      shipLayer.addLayer(marker)

      // Floating time label above ship
      const arrivalMs = ship.arriving_at ? new Date(ship.arriving_at).getTime() - now : 0
      const timeText = ship.arriving_at ? formatTimeRemaining(arrivalMs) : 'In transit'
      const label = L.marker(pos, {
        icon: L.divIcon({
          className: 'ship-time-label',
          html: `<span style="color:${strategyColor}">${timeText}</span>`,
          iconSize: [0, 0],
          iconAnchor: [0, 18],
        }),
        interactive: false,
      })
      shipLayer.addLayer(label)

      travelingShipMarkers.set(ship.ship_id, { marker, label, trail, path: seaPath, ship })
    }
  }
}

// Smoothly update traveling ship positions every second without a full redraw.
function animateShips() {
  if (travelingShipMarkers.size === 0) return
  const routeLookup = buildRouteLookup()
  const now = Date.now()

  for (const [, entry] of travelingShipMarkers) {
    const progress = computeShipProgress(entry.ship, routeLookup, now)
    const pos = interpolateAlongPath(entry.path, progress)
    const trailPath = getTrailPath(entry.path, progress)
    const latLng = L.latLng(pos[0], pos[1])

    entry.marker.setLatLng(latLng)
    entry.label.setLatLng(latLng)
    entry.trail.setLatLngs(trailPath)

    const arrivalMs = entry.ship.arriving_at ? new Date(entry.ship.arriving_at).getTime() - now : 0
    const timeText = entry.ship.arriving_at ? formatTimeRemaining(arrivalMs) : 'In transit'
    const strategyColor = STRATEGY_COLORS[entry.ship.strategy] ?? '#94a3b8'
    entry.label.setIcon(L.divIcon({
      className: 'ship-time-label',
      html: `<span style="color:${strategyColor}">${timeText}</span>`,
      iconSize: [0, 0],
      iconAnchor: [0, 18],
    }))
  }
}

function formatCurrency(value: number): string {
  return new Intl.NumberFormat('en-US').format(value)
}

function shipEta(ship: MapShip): string {
  if (!ship.arriving_at) return ''
  const ms = new Date(ship.arriving_at).getTime() - Date.now()
  return ms > 0 ? formatTimeRemaining(ms) : 'Arriving'
}

function buildShipPopup(ship: MapShip): string {
  const strategyColor = STRATEGY_COLORS[ship.strategy] ?? '#94a3b8'

  let statusHtml = ''
  if (ship.status === 'docked') {
    statusHtml = `Docked${ship.port_name ? ` at ${ship.port_name}` : ''}`
  } else {
    statusHtml = `${ship.from_port_name || '?'} → ${ship.to_port_name || '?'}`
    const eta = shipEta(ship)
    if (eta) statusHtml += `<br><span style="color:#94a3b8">ETA:</span> ${eta}`
  }

  const cargoPercent = ship.capacity > 0 ? Math.round((ship.cargo_total / ship.capacity) * 100) : 0
  const cargoBarColor = cargoPercent > 80 ? '#f59e0b' : cargoPercent > 50 ? '#3b82f6' : '#10b981'

  let cargoListHtml = '<div style="color:#64748b;font-style:italic">Empty hold</div>'
  if (ship.cargo && ship.cargo.length > 0) {
    cargoListHtml = ship.cargo.map(item =>
      `<div style="display:flex;justify-content:space-between;gap:12px">
        <span style="color:#94a3b8">${item.good_name}</span>
        <span style="color:#e2e8f0;font-family:monospace">${item.quantity}</span>
      </div>`,
    ).join('')
  }

  return `<div style="min-width:220px;font-size:12px;line-height:1.5">
    <div style="font-weight:700;font-size:13px;margin-bottom:2px;color:#f1f5f9">${ship.ship_name}</div>
    <div style="display:flex;align-items:center;gap:6px;margin-bottom:6px">
      <span style="display:inline-block;width:8px;height:8px;border-radius:50%;background:${strategyColor}"></span>
      <span style="color:${strategyColor};font-weight:600">${ship.strategy}</span>
      <span style="color:#475569">·</span>
      <span style="color:#94a3b8">${ship.company_name}</span>
    </div>
    <div style="color:#cbd5e1;margin-bottom:8px">${statusHtml}</div>

    <div style="display:grid;grid-template-columns:1fr 1fr 1fr;gap:4px;margin-bottom:8px">
      <div style="background:#1e293b;border-radius:4px;padding:4px;text-align:center">
        <div style="font-size:9px;color:#64748b;text-transform:uppercase">Speed</div>
        <div style="color:#e2e8f0;font-family:monospace;font-weight:600">${ship.speed || '---'}</div>
      </div>
      <div style="background:#1e293b;border-radius:4px;padding:4px;text-align:center">
        <div style="font-size:9px;color:#64748b;text-transform:uppercase">Upkeep/cycle</div>
        <div style="color:#e2e8f0;font-family:monospace;font-weight:600">${ship.upkeep ? formatCurrency(ship.upkeep) : '---'}</div>
      </div>
      <div style="background:#1e293b;border-radius:4px;padding:4px;text-align:center">
        <div style="font-size:9px;color:#64748b;text-transform:uppercase">Pax</div>
        <div style="color:#c084fc;font-family:monospace;font-weight:600">${ship.passenger_count}/${ship.passenger_cap}</div>
      </div>
    </div>

    <div style="margin-bottom:4px">
      <div style="display:flex;justify-content:space-between;margin-bottom:3px">
        <span style="color:#64748b;font-weight:600">Cargo</span>
        <span style="color:#94a3b8;font-family:monospace">${ship.cargo_total} / ${ship.capacity}</span>
      </div>
      <div style="background:#1e293b;border-radius:3px;height:4px;overflow:hidden;margin-bottom:6px">
        <div style="background:${cargoBarColor};height:100%;width:${cargoPercent}%;border-radius:3px;transition:width 0.3s"></div>
      </div>
      ${cargoListHtml}
    </div>
  </div>`
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
  worldPollTimer = setInterval(fetchWorldData, 60_000) // Refresh ports/routes every 60s
  animTimer = setInterval(animateShips, 1_000)
})

onUnmounted(() => {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
  if (worldPollTimer) {
    clearInterval(worldPollTimer)
    worldPollTimer = null
  }
  if (animTimer) {
    clearInterval(animTimer)
    animTimer = null
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

    <div v-if="loading" class="h-[calc(100vh-12rem)] min-h-[400px] flex items-center justify-center">
      <Icon name="mdi:loading" class="animate-spin text-2xl text-slate-500" />
    </div>

    <div class="relative">
      <div ref="mapContainer" class="h-[calc(100vh-12rem)] min-h-[400px] rounded-lg overflow-hidden" />

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
  box-shadow: 0 10px 25px rgba(0, 0, 0, 0.5);
}

:deep(.leaflet-popup-content) {
  margin: 10px 14px;
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

:deep(.leaflet-container) {
  background-color: #0f172a;
}

.port-label span {
  color: #cbd5e1;
  font-size: 11px;
  font-weight: 600;
  text-shadow: 0 1px 3px rgba(0, 0, 0, 0.8), 0 0 6px rgba(0, 0, 0, 0.6);
  white-space: nowrap;
}

:deep(.ship-time-label) span {
  font-size: 10px;
  font-weight: 700;
  white-space: nowrap;
  text-shadow: 0 1px 3px rgba(0, 0, 0, 0.9), 0 0 6px rgba(0, 0, 0, 0.7);
}
</style>

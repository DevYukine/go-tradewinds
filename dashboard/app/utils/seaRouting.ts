/**
 * Sea routing for the world map — routes ships around landmasses instead of
 * through them. Uses a sparse waypoint graph covering European waters with
 * A* pathfinding. Routes are cached after first computation.
 */

export type SeaPoint = [number, number] // [lat, lng]

// ---------------------------------------------------------------------------
// Simplified land polygons (clockwise vertex order)
// Slightly inset from real coastlines so narrow straits aren't flagged.
// ---------------------------------------------------------------------------

const GREAT_BRITAIN: SeaPoint[] = [
  // South coast W→E
  [50.0, -5.2], [50.3, -3.6], [50.5, -2.4], [50.7, -0.8],
  [50.7, 0.3], [51.0, 1.0], [51.4, 1.4],
  // East coast S→N
  [51.8, 1.5], [52.7, 1.2], [53.2, 0.2], [53.6, -0.1],
  [54.4, -1.0], [55.5, -1.6],
  // NE Scotland
  [56.2, -2.0], [57.2, -1.8], [58.4, -3.2],
  // North
  [58.3, -4.8],
  // West coast N→S
  [57.4, -5.4], [56.3, -5.4], [55.1, -5.0],
  [54.3, -3.2], [53.3, -3.0],
  // Wales & SW
  [51.6, -3.2], [51.0, -3.8], [50.4, -4.9],
]

const IRELAND: SeaPoint[] = [
  [51.4, -9.5], [51.8, -7.5], [52.2, -6.2], [53.2, -5.9],
  [54.1, -5.8], [55.1, -5.9], [55.3, -7.3],
  [54.4, -9.8], [52.9, -10.2], [51.7, -9.8],
]

const JUTLAND: SeaPoint[] = [
  [54.8, 8.7], [55.4, 8.1], [56.4, 8.2], [57.4, 9.8],
  [57.0, 10.5], [56.1, 10.3], [55.2, 9.6], [54.8, 9.5],
]

const SOUTH_NORWAY: SeaPoint[] = [
  [57.9, 6.5], [58.7, 5.5], [59.4, 5.2], [60.4, 5.1],
  [61.0, 5.5], [61.0, 8.0], [60.0, 10.0],
  [59.2, 10.8], [58.9, 10.5], [58.4, 8.0], [57.9, 7.0],
]

const SWEDEN: SeaPoint[] = [
  [55.4, 12.8], [55.4, 14.5], [56.0, 16.0],
  [57.5, 16.5], [58.5, 16.5], [59.5, 18.2],
  [60.5, 17.5], [63.0, 18.5], [63.0, 14.0],
  [61.0, 14.0], [59.5, 12.0], [58.3, 11.2],
  [57.0, 12.0], [56.0, 12.5], [55.9, 11.2],
  [55.4, 12.5],
]

const FRANCE: SeaPoint[] = [
  [51.0, 1.8], [49.0, 2.5], [48.8, 3.0],
  [47.5, 1.0], [47.0, -1.0], [46.3, -1.2],
  [45.5, -1.1], [44.4, -1.2], [43.3, -1.6],
  [42.5, 0.0], [42.5, 3.0], [43.2, 3.5],
  [43.2, 5.5], [43.5, 6.5], [43.7, 7.3],
  [46.0, 7.0], [48.0, 7.5], [49.0, 8.0],
  [50.0, 4.0], [51.0, 3.0],
]

const IBERIA: SeaPoint[] = [
  [43.4, -1.6], [43.4, -8.8], [42.0, -8.8],
  [38.7, -9.4], [37.0, -8.9], [36.1, -5.5],
  [36.4, -2.0], [37.5, -1.5], [38.5, 0.0],
  [39.9, 0.3], [40.5, 0.5], [41.2, 1.0],
  [42.3, 3.1], [42.5, 0.0], [43.1, -1.5],
]

const ITALY: SeaPoint[] = [
  [44.0, 7.5], [43.8, 8.0], [44.3, 9.0],
  [44.3, 12.5], [43.5, 13.5], [42.0, 14.0],
  [41.0, 15.5], [40.0, 16.0], [38.2, 15.6],
  [37.9, 12.5], [38.0, 13.0], [39.0, 16.5],
  [40.5, 18.5], [42.0, 15.0], [43.0, 12.5],
  [44.5, 12.3], [45.5, 13.5], [46.5, 13.5],
  [46.0, 10.0], [45.5, 7.5],
]

const SICILY: SeaPoint[] = [
  [38.3, 12.4], [38.2, 13.0], [37.5, 15.1],
  [37.0, 15.3], [37.0, 14.0], [37.5, 12.8],
]

const CORSICA_SARDINIA: SeaPoint[] = [
  [43.0, 9.0], [42.7, 9.5], [41.4, 9.2],
  [41.0, 8.2], [39.2, 8.2], [38.8, 8.5],
  [38.8, 9.6], [39.2, 9.8], [40.5, 9.8],
  [41.0, 9.8], [41.3, 9.5], [42.0, 9.3],
  [42.9, 9.6], [43.0, 9.2],
]

const BALKANS: SeaPoint[] = [
  [46.5, 13.5], [45.5, 15.0], [44.0, 15.5],
  [42.5, 18.5], [39.5, 20.0], [38.0, 21.5],
  [36.5, 22.5], [36.0, 23.0], [37.0, 24.0],
  [38.0, 24.0], [39.0, 23.5], [40.5, 23.0],
  [41.0, 26.0], [42.0, 28.0], [43.0, 28.5],
  [44.5, 29.0], [46.0, 30.0], [48.0, 25.0],
  [47.5, 17.0], [47.0, 15.0],
]

const TURKEY: SeaPoint[] = [
  [41.0, 26.0], [40.5, 26.5], [40.0, 26.5],
  [39.0, 26.5], [38.5, 26.0], [37.0, 27.5],
  [36.0, 29.0], [36.0, 36.0], [37.5, 36.5],
  [40.0, 35.0], [41.5, 32.0], [42.0, 35.0],
  [41.5, 36.0], [41.0, 40.0], [42.0, 42.0],
  [42.0, 29.0], [41.5, 28.5], [41.2, 29.0],
]

const AFRICA_NORTH: SeaPoint[] = [
  [37.5, -1.0], [36.8, -2.0], [35.8, -5.3],
  [35.2, -5.5], [34.0, -6.5], [33.0, -8.0],
  [33.0, 11.0], [34.0, 11.5], [37.0, 10.5],
  [37.2, 9.5], [36.5, 8.5], [36.8, 3.0],
  [36.5, 1.0],
]

const LAND_POLYGONS = [
  GREAT_BRITAIN, IRELAND, JUTLAND, SOUTH_NORWAY, SWEDEN,
  FRANCE, IBERIA, ITALY, SICILY, CORSICA_SARDINIA,
  BALKANS, TURKEY, AFRICA_NORTH,
]

// ---------------------------------------------------------------------------
// Sea waypoints — placed in navigable water throughout European seas
// ---------------------------------------------------------------------------

const WAYPOINTS: SeaPoint[] = [
  // English Channel
  [49.3, -5.5],    // 0  Western Approaches
  [49.5, -2.5],    // 1  Channel W
  [49.8, -0.5],    // 2  Channel C
  [50.3, 0.5],     // 3  Channel E
  [51.0, 1.6],     // 4  Dover Strait

  // Southern North Sea
  [51.5, 2.5],     // 5  Off Thames
  [52.0, 3.5],     // 6  Off Netherlands S
  [52.5, 4.5],     // 7  Off Netherlands

  // Central North Sea
  [53.5, 4.0],     // 8  Off Frisians
  [54.5, 5.0],     // 9  Central NS
  [55.5, 5.0],     // 10 Central NS N

  // Northern North Sea
  [56.5, 3.0],     // 11 Off E Scotland
  [57.5, 0.5],     // 12 Off Aberdeen
  [58.5, -0.5],    // 13 Off NE Scotland
  [59.5, 1.0],     // 14 N North Sea

  // Norwegian coast
  [58.0, 5.5],     // 15 Off Stavanger
  [59.5, 4.5],     // 16 Off Bergen
  [60.5, 3.5],     // 17 Norwegian Sea
  [61.5, 3.0],     // 18 Norwegian Sea N

  // Skagerrak
  [57.5, 7.5],     // 19 Skagerrak W
  [57.8, 9.0],     // 20 Skagerrak E

  // Kattegat
  [57.0, 11.5],    // 21 Kattegat N
  [56.3, 11.5],    // 22 Kattegat C
  [55.5, 12.5],    // 23 Øresund

  // Baltic
  [55.0, 11.0],    // 24 Great Belt
  [54.5, 12.0],    // 25 W Baltic
  [54.5, 14.0],    // 26 Off Rügen
  [55.5, 16.0],    // 27 S Baltic
  [55.0, 18.0],    // 28 Central Baltic S
  [57.0, 19.5],    // 29 Central Baltic
  [59.0, 21.0],    // 30 Gulf of Finland W
  [60.0, 24.5],    // 31 Gulf of Finland
  [57.5, 24.0],    // 32 Off Riga

  // German Bight
  [54.0, 7.5],     // 33 German Bight W
  [54.0, 8.0],     // 34 German Bight E

  // Bay of Biscay
  [48.0, -5.5],    // 35 Off Brittany
  [47.0, -3.0],    // 36 Biscay NE
  [46.0, -2.5],    // 37 Biscay C
  [44.5, -2.5],    // 38 Biscay S
  [43.5, -3.0],    // 39 Off Bilbao

  // Atlantic W of UK
  [49.5, -7.0],    // 40 Off Cornwall SW
  [51.0, -7.5],    // 41 Off S Ireland
  [52.5, -8.0],    // 42 Off W Ireland
  [54.0, -8.5],    // 43 Off NW Ireland
  [55.5, -8.0],    // 44 Off Donegal
  [57.0, -7.5],    // 45 Hebrides
  [58.5, -6.0],    // 46 Off NW Scotland
  [59.5, -3.5],    // 47 Off Orkney

  // Irish Sea
  [51.5, -6.0],    // 48 St George's Channel
  [53.0, -5.5],    // 49 Irish Sea S
  [54.0, -5.0],    // 50 Irish Sea C
  [54.5, -4.5],    // 51 Irish Sea N

  // Atlantic / Iberia
  [43.0, -9.5],    // 52 Off NW Spain
  [41.0, -9.5],    // 53 Off Porto
  [38.5, -9.8],    // 54 Off Lisbon
  [36.5, -7.5],    // 55 Off S Portugal
  [36.0, -5.5],    // 56 Strait of Gibraltar W

  // Western Mediterranean
  [36.5, -3.0],    // 57 Alboran Sea
  [37.0, -1.0],    // 58 Off SE Spain
  [38.5, -0.5],    // 59 Off Valencia
  [39.5, 0.5],     // 60 Off E Spain
  [41.0, 1.5],     // 61 Off Barcelona
  [42.0, 3.5],     // 62 Gulf of Lion

  // Central Mediterranean
  [43.0, 6.0],     // 63 Off French Riviera
  [43.5, 7.5],     // 64 Off Nice/Genoa approach
  [43.5, 9.5],     // 65 Ligurian Sea (W of Corsica)
  [41.5, 9.0],     // 66 W of Corsica/Sardinia
  [39.0, 7.0],     // 67 SW of Sardinia
  [42.0, 11.0],    // 68 Tyrrhenian N
  [40.5, 12.5],    // 69 Tyrrhenian C
  [39.0, 13.0],    // 70 Tyrrhenian S

  // Southern Mediterranean
  [38.0, 11.5],    // 71 Off W Sicily
  [37.0, 12.0],    // 72 S of Sicily
  [36.5, 15.0],    // 73 Off E Sicily
  [37.5, 18.0],    // 74 Ionian Sea
  [35.0, 24.0],    // 75 S of Crete
  [37.0, 25.5],    // 76 Aegean S
  [38.5, 25.0],    // 77 Aegean C
  [40.0, 25.0],    // 78 Aegean N

  // Eastern Mediterranean
  [40.5, 29.0],    // 79 Sea of Marmara approach
  [33.0, 29.0],    // 80 Off Egypt
  [34.5, 11.0],    // 81 Off Tunisia E
  [37.0, 10.0],    // 82 Off Tunisia N
  [36.0, 1.0],     // 83 Off Algeria
  [35.5, -5.0],    // 84 Off Tangier

  // Adriatic
  [44.5, 13.0],    // 85 N Adriatic
  [42.5, 16.0],    // 86 C Adriatic
  [40.0, 18.5],    // 87 S Adriatic

  // North Africa coast
  [36.0, -3.0],    // 88 Off N Africa W
]

// ---------------------------------------------------------------------------
// Waypoint graph edges — each pair is guaranteed to not cross major land
// ---------------------------------------------------------------------------

const EDGES: [number, number][] = [
  // English Channel chain
  [0, 1], [1, 2], [2, 3], [3, 4],
  // Channel → North Sea
  [4, 5], [5, 6], [6, 7],
  // North Sea chain
  [7, 8], [8, 9], [9, 10], [10, 11], [11, 12], [12, 13], [13, 14],
  // Norwegian coast
  [10, 15], [15, 16], [16, 17], [17, 18], [14, 16], [9, 15],
  // Skagerrak
  [10, 19], [15, 19], [19, 20],
  // Skagerrak → Kattegat
  [20, 21], [21, 22], [22, 23],
  // Kattegat → Baltic
  [23, 24], [24, 25], [25, 26], [26, 27], [27, 28],
  // Baltic
  [28, 29], [29, 30], [30, 31], [29, 32], [27, 29],
  // German Bight
  [8, 33], [33, 34], [34, 19], [9, 33],
  // Bay of Biscay
  [0, 35], [35, 36], [36, 37], [37, 38], [38, 39],
  // Atlantic W of UK
  [0, 40], [40, 41], [41, 42], [42, 43], [43, 44], [44, 45], [45, 46], [46, 47],
  // Orkney → N North Sea
  [47, 14], [47, 13], [46, 13],
  // Irish Sea
  [41, 48], [48, 49], [49, 50], [50, 51], [51, 44],
  // Atlantic / Iberia
  [39, 52], [52, 53], [53, 54], [54, 55], [55, 56],
  [40, 52],
  // Strait of Gibraltar
  [56, 84], [84, 57], [84, 88],
  // Western Med
  [57, 58], [58, 59], [59, 60], [60, 61], [61, 62],
  [88, 57], [57, 83],
  // Central Med
  [62, 63], [63, 64], [64, 65], [65, 66], [66, 67],
  [65, 68], [68, 69], [69, 70],
  // Southern Med
  [70, 71], [71, 72], [72, 73], [73, 74],
  [74, 75], [75, 76], [76, 77], [77, 78],
  // Eastern Med
  [78, 79],
  [75, 80], [80, 81], [81, 82], [82, 83],
  // Adriatic
  [85, 86], [86, 87], [87, 74],
  // Cross-links
  [72, 82], [73, 82], [67, 72],
  [83, 88],
  // N Italy coast → Adriatic
  [64, 85],
  // Bay of Biscay → Iberia
  [35, 0], [40, 0],
  // Cross-channel shortcuts
  [3, 5],
]

// ---------------------------------------------------------------------------
// Geometry helpers
// ---------------------------------------------------------------------------

function pointInPolygon(point: SeaPoint, polygon: SeaPoint[]): boolean {
  let inside = false
  const [y, x] = point
  for (let i = 0, j = polygon.length - 1; i < polygon.length; j = i++) {
    const pi = polygon[i]!
    const pj = polygon[j]!
    if ((pi[0] > y) !== (pj[0] > y) && x < ((pj[1] - pi[1]) * (y - pi[0])) / (pj[0] - pi[0]) + pi[1]) {
      inside = !inside
    }
  }
  return inside
}

function isOnLand(p: SeaPoint): boolean {
  for (const poly of LAND_POLYGONS) {
    if (pointInPolygon(p, poly)) return true
  }
  return false
}

/** Sample the segment at N interior points; skip first/last 10% (port may be on land). */
function segmentCrossesLand(from: SeaPoint, to: SeaPoint): boolean {
  const steps = 20
  for (let i = 2; i <= steps - 2; i++) {
    const t = i / steps
    const lat = from[0] + (to[0] - from[0]) * t
    const lng = from[1] + (to[1] - from[1]) * t
    if (isOnLand([lat, lng])) return true
  }
  return false
}

function eucDist(a: SeaPoint, b: SeaPoint): number {
  const dlat = a[0] - b[0]
  const dlng = a[1] - b[1]
  return Math.sqrt(dlat * dlat + dlng * dlng)
}

// ---------------------------------------------------------------------------
// A* pathfinding on the waypoint graph
// ---------------------------------------------------------------------------

/** Build adjacency list from EDGES, adding distance weights. */
function buildAdjacency(): Map<number, { to: number; cost: number }[]> {
  const adj = new Map<number, { to: number; cost: number }[]>()
  for (const [a, b] of EDGES) {
    const wa = WAYPOINTS[a]
    const wb = WAYPOINTS[b]
    if (!wa || !wb) continue
    const cost = eucDist(wa, wb)
    if (!adj.has(a)) adj.set(a, [])
    if (!adj.has(b)) adj.set(b, [])
    adj.get(a)!.push({ to: b, cost })
    adj.get(b)!.push({ to: a, cost })
  }
  return adj
}

const adjacency = buildAdjacency()

/**
 * Find nearest waypoints reachable from a point (line of sight doesn't cross land).
 * Returns indices sorted by distance.
 */
function nearestReachableWaypoints(point: SeaPoint, maxCount: number = 4): number[] {
  const scored = WAYPOINTS
    .map((wp, idx) => ({ idx, dist: eucDist(point, wp) }))
    .sort((a, b) => a.dist - b.dist)

  const result: number[] = []
  // Check up to 15 nearest waypoints to find enough clear ones
  const checkLimit = Math.min(scored.length, 15)
  for (let i = 0; i < checkLimit; i++) {
    if (result.length >= maxCount) break
    const { idx } = scored[i]!
    if (!segmentCrossesLand(point, WAYPOINTS[idx]!)) {
      result.push(idx)
    }
  }

  // Fallback: if no clear LOS found, take the 2 nearest regardless
  if (result.length === 0) {
    return scored.slice(0, 2).map(s => s.idx)
  }

  return result
}

/** A* search from startIdx to goalIdx in the waypoint graph. Returns path indices or null. */
function astar(startIdx: number, goalIdx: number, extraEdges: Map<number, { to: number; cost: number }[]>): number[] | null {
  const goal = WAYPOINTS[goalIdx] ?? [0, 0]

  const gScore = new Map<number, number>()
  const fScore = new Map<number, number>()
  const cameFrom = new Map<number, number>()
  const open = new Set<number>()

  gScore.set(startIdx, 0)
  fScore.set(startIdx, eucDist(WAYPOINTS[startIdx] ?? [0, 0], goal))
  open.add(startIdx)

  while (open.size > 0) {
    // Find node in open with lowest fScore
    let current = -1
    let bestF = Infinity
    for (const n of open) {
      const f = fScore.get(n) ?? Infinity
      if (f < bestF) {
        bestF = f
        current = n
      }
    }
    if (current === -1) break

    if (current === goalIdx) {
      // Reconstruct path
      const path: number[] = [current]
      let c = current
      while (cameFrom.has(c)) {
        c = cameFrom.get(c)!
        path.unshift(c)
      }
      return path
    }

    open.delete(current)

    // Gather neighbours from both main adjacency and extra edges
    const neighbours: { to: number; cost: number }[] = [
      ...(adjacency.get(current) ?? []),
      ...(extraEdges.get(current) ?? []),
    ]

    for (const { to, cost } of neighbours) {
      const tentG = (gScore.get(current) ?? Infinity) + cost
      if (tentG < (gScore.get(to) ?? Infinity)) {
        cameFrom.set(to, current)
        gScore.set(to, tentG)
        const wp = WAYPOINTS[to] ?? [0, 0]
        fScore.set(to, tentG + eucDist(wp, goal))
        open.add(to)
      }
    }
  }

  return null
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

const routeCache = new Map<string, SeaPoint[]>()

function cacheKey(from: SeaPoint, to: SeaPoint): string {
  return `${from[0].toFixed(3)},${from[1].toFixed(3)}>${to[0].toFixed(3)},${to[1].toFixed(3)}`
}

/**
 * Compute a sea route between two points, routing around landmasses.
 * Returns an array of [lat, lng] waypoints forming the path.
 * Results are cached — cheap to call repeatedly for the same from/to.
 */
export function computeSeaRoute(from: SeaPoint, to: SeaPoint): SeaPoint[] {
  const key = cacheKey(from, to)
  if (routeCache.has(key)) return routeCache.get(key)!

  // Fast path: straight line doesn't cross land
  if (!segmentCrossesLand(from, to)) {
    const result = [from, to]
    routeCache.set(key, result)
    return result
  }

  // Use virtual node indices beyond the normal waypoint range
  const fromVirtIdx = WAYPOINTS.length
  const toVirtIdx = WAYPOINTS.length + 1

  // Temporarily "place" from/to as virtual waypoints
  const virtualWaypoints = [...WAYPOINTS, from, to]

  // Connect virtual nodes to nearest reachable waypoints
  const extraEdges = new Map<number, { to: number; cost: number }[]>()

  const fromNeighbours = nearestReachableWaypoints(from)
  const toNeighbours = nearestReachableWaypoints(to)

  extraEdges.set(fromVirtIdx, fromNeighbours.map(idx => ({
    to: idx,
    cost: eucDist(from, WAYPOINTS[idx]!),
  })))

  for (const idx of fromNeighbours) {
    if (!extraEdges.has(idx)) extraEdges.set(idx, [])
    extraEdges.get(idx)!.push({ to: fromVirtIdx, cost: eucDist(WAYPOINTS[idx]!, from) })
  }

  extraEdges.set(toVirtIdx, toNeighbours.map(idx => ({
    to: idx,
    cost: eucDist(to, WAYPOINTS[idx]!),
  })))

  for (const idx of toNeighbours) {
    if (!extraEdges.has(idx)) extraEdges.set(idx, [])
    extraEdges.get(idx)!.push({ to: toVirtIdx, cost: eucDist(WAYPOINTS[idx]!, to) })
  }

  // Patch WAYPOINTS temporarily for A* coordinate lookups
  const origLength = WAYPOINTS.length
  ;(WAYPOINTS as SeaPoint[]).push(from, to)

  const pathIndices = astar(fromVirtIdx, toVirtIdx, extraEdges)

  // Restore WAYPOINTS
  WAYPOINTS.length = origLength

  if (!pathIndices) {
    // Fallback: direct line if no path found
    const result = [from, to]
    routeCache.set(key, result)
    return result
  }

  const result: SeaPoint[] = pathIndices.map(idx => {
    if (idx === fromVirtIdx) return from
    if (idx === toVirtIdx) return to
    return WAYPOINTS[idx]!
  })

  routeCache.set(key, result)
  return result
}

/**
 * Interpolate a position along a multi-segment path at progress t ∈ [0, 1].
 */
export function interpolateAlongPath(path: SeaPoint[], t: number): SeaPoint {
  if (path.length < 2) return path[0]!
  if (t <= 0) return path[0]!
  if (t >= 1) return path[path.length - 1]!

  // Compute cumulative segment lengths
  let totalLength = 0
  const lengths: number[] = []
  for (let i = 1; i < path.length; i++) {
    const l = eucDist(path[i - 1]!, path[i]!)
    lengths.push(l)
    totalLength += l
  }

  let targetDist = t * totalLength
  for (let i = 0; i < lengths.length; i++) {
    const curr = path[i]!
    const next = path[i + 1]!
    const len = lengths[i]!
    if (targetDist <= len) {
      const segT = len > 0 ? targetDist / len : 0
      return [
        curr[0] + (next[0] - curr[0]) * segT,
        curr[1] + (next[1] - curr[1]) * segT,
      ]
    }
    targetDist -= len
  }

  return path[path.length - 1]!
}

/**
 * Get the partial path from the start up to position t ∈ [0, 1].
 * Used for drawing the ship trail along the route.
 */
export function getTrailPath(path: SeaPoint[], t: number): SeaPoint[] {
  if (path.length < 2 || t <= 0) return [path[0]!]
  if (t >= 1) return [...path]

  let totalLength = 0
  const lengths: number[] = []
  for (let i = 1; i < path.length; i++) {
    const l = eucDist(path[i - 1]!, path[i]!)
    lengths.push(l)
    totalLength += l
  }

  let targetDist = t * totalLength
  const result: SeaPoint[] = [path[0]!]

  for (let i = 0; i < lengths.length; i++) {
    const curr = path[i]!
    const next = path[i + 1]!
    if (targetDist <= lengths[i]!) {
      const segT = lengths[i]! > 0 ? targetDist / lengths[i]! : 0
      result.push([
        curr[0] + (next[0] - curr[0]) * segT,
        curr[1] + (next[1] - curr[1]) * segT,
      ])
      break
    }
    result.push(next)
    targetDist -= lengths[i]!
  }

  return result
}

interface StateEvent {
  type: string
  ts: number
}

type EventHandler = (event: StateEvent) => void

// Global tracking: one SSE connection per company, shared across all subscribers.
const connections = new Map<number, {
  source: EventSource
  handlers: Set<EventHandler>
}>()

export function useCompanyEvents() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  let boundCompanyId: number | null = null
  let boundHandler: EventHandler | null = null

  function connect(companyId: number, handler: EventHandler) {
    disconnect()

    boundCompanyId = companyId
    boundHandler = handler

    let conn = connections.get(companyId)
    if (!conn) {
      const source = new EventSource(`${apiBase}/sse/events/${companyId}`)
      conn = { source, handlers: new Set() }
      connections.set(companyId, conn)

      source.onmessage = (msg) => {
        try {
          const event: StateEvent = JSON.parse(msg.data)
          const c = connections.get(companyId)
          if (c) {
            for (const h of c.handlers) {
              h(event)
            }
          }
        } catch { /* ignore parse errors */ }
      }

      source.onerror = () => {
        // EventSource auto-reconnects by default.
      }
    }

    conn.handlers.add(handler)
  }

  function disconnect() {
    if (boundCompanyId === null || boundHandler === null) return

    const conn = connections.get(boundCompanyId)
    if (conn) {
      conn.handlers.delete(boundHandler)
      if (conn.handlers.size === 0) {
        conn.source.close()
        connections.delete(boundCompanyId)
      }
    }

    boundCompanyId = null
    boundHandler = null
  }

  onUnmounted(() => disconnect())

  return { connect, disconnect }
}

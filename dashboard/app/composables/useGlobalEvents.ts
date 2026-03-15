interface GlobalEvent {
  type: string
  ts: number
  company_id: number
}

type GlobalEventHandler = (event: GlobalEvent) => void

// Singleton SSE connection shared across all subscribers.
let source: EventSource | null = null
const handlers = new Set<GlobalEventHandler>()
let subscriberCount = 0

export function useGlobalEvents() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  let boundHandler: GlobalEventHandler | null = null

  function subscribe(handler: GlobalEventHandler) {
    unsubscribe()

    boundHandler = handler
    handlers.add(handler)
    subscriberCount++

    if (!source) {
      source = new EventSource(`${apiBase}/sse/global-events`)

      source.onmessage = (msg) => {
        try {
          const event: GlobalEvent = JSON.parse(msg.data)
          for (const h of handlers) {
            h(event)
          }
        } catch { /* ignore parse errors */ }
      }

      source.onerror = () => {
        // EventSource auto-reconnects by default.
      }
    }
  }

  function unsubscribe() {
    if (!boundHandler) return

    handlers.delete(boundHandler)
    subscriberCount--
    boundHandler = null

    if (subscriberCount <= 0 && source) {
      source.close()
      source = null
      subscriberCount = 0
    }
  }

  onUnmounted(() => unsubscribe())

  return { subscribe, unsubscribe }
}

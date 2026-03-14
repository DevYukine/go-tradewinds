import type { LogEntry } from '~/types'

const MAX_ENTRIES = 500

export function useLogs() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const logs = ref<LogEntry[]>([])
  const paused = ref(false)
  const connected = ref(false)
  let eventSource: EventSource | null = null
  let buffer: LogEntry[] = []

  function connectSSE(companyId: number) {
    disconnectSSE()
    logs.value = []
    buffer = []

    eventSource = new EventSource(`${apiBase}/sse/logs/${companyId}`)
    connected.value = true

    eventSource.onmessage = (event) => {
      try {
        const entry: LogEntry = JSON.parse(event.data)
        if (paused.value) {
          buffer.push(entry)
        } else {
          logs.value.push(entry)
          if (logs.value.length > MAX_ENTRIES) {
            logs.value = logs.value.slice(-MAX_ENTRIES)
          }
        }
      } catch (e) {
        console.error('Failed to parse SSE log data:', e)
      }
    }

    eventSource.onerror = () => {
      connected.value = false
      console.error('Log SSE connection error')
    }
  }

  function disconnectSSE() {
    if (eventSource) {
      eventSource.close()
      eventSource = null
    }
    connected.value = false
  }

  function togglePause() {
    paused.value = !paused.value
    if (!paused.value && buffer.length > 0) {
      logs.value.push(...buffer)
      buffer = []
      if (logs.value.length > MAX_ENTRIES) {
        logs.value = logs.value.slice(-MAX_ENTRIES)
      }
    }
  }

  onUnmounted(() => {
    disconnectSSE()
  })

  return {
    logs,
    paused,
    connected,
    connectSSE,
    disconnectSSE,
    togglePause,
  }
}

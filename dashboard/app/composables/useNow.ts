// Reactive timestamp that ticks every second for live countdowns.
export function useNow(intervalMs = 1000) {
  const now = ref(Date.now())
  let timer: ReturnType<typeof setInterval> | null = null

  onMounted(() => {
    timer = setInterval(() => {
      now.value = Date.now()
    }, intervalMs)
  })

  onUnmounted(() => {
    if (timer) clearInterval(timer)
  })

  return { now }
}

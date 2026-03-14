import type { Config } from 'tailwindcss'

export default {
  theme: {
    extend: {
      colors: {
        navy: {
          900: '#0a0e1a',
          800: '#111827',
          700: '#1e293b',
        },
        profit: '#10b981',
        loss: '#f43f5e',
        status: {
          running: '#10b981',
          paused: '#eab308',
          error: '#f43f5e',
          bankrupt: '#6b7280',
        },
        log: {
          info: '#94a3b8',
          warn: '#eab308',
          error: '#f43f5e',
          trade: '#10b981',
          event: '#3b82f6',
          agent: '#a855f7',
          optimizer: '#f97316',
        },
      },
      fontFamily: {
        sans: ['"Inter"', 'ui-sans-serif', 'system-ui', 'sans-serif'],
      },
    },
  },
} satisfies Config

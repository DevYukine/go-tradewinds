export default defineNuxtConfig({
  ssr: false,

  modules: [
    '@nuxtjs/tailwindcss',
    '@nuxt/icon',
    'nuxt-charts',
  ],

  runtimeConfig: {
    public: {
      apiBase: 'http://localhost:3001',
    },
  },

  icon: {
    serverBundle: 'remote',
  },

  compatibilityDate: '2025-01-01',
})

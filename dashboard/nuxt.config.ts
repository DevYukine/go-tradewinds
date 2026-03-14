export default defineNuxtConfig({
  ssr: false,

  modules: [
    '@nuxtjs/tailwindcss',
    '@nuxt/icon',
    'nuxt-charts',
  ],

  runtimeConfig: {
    public: {
      apiBase: 'http://127.0.0.1:3002',
    },
  },

  icon: {
    serverBundle: 'remote',
  },

  compatibilityDate: '2025-01-01',
})

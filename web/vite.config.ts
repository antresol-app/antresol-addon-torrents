import { defineConfig } from 'vite'
import { antresolAddon } from '@antresol/addon-sdk/vite-plugin'

// dist/web/index.js on the app's React, router and SDK (docs/addons/contract.md)
export default defineConfig({ plugins: [antresolAddon()] })

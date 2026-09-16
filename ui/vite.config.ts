import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

// В dev API проксируется на локальный Go. Production — только статические
// файлы под /ui/; UI и API обслуживаются одним origin без CORS и Node-сервера.
const backend = process.env.LAWA_UI_BACKEND || 'http://127.0.0.1:60800';
export default defineConfig({
  plugins: [react()],
  base: '/',
  build: {
    outDir: '../internal/dashboard/web',
    emptyOutDir: true,
    assetsDir: 'ui/assets',
    license: { fileName: 'ui/licenses.json' },
  },
  server: {
    proxy: Object.fromEntries(
      ['/api', '/memory', '/events', '/uml', '/graph-image', '/assets'].map(
        (path) => [path, backend],
      ),
    ),
  },
  test: {
    environment: 'jsdom',
    // UIKit импортирует CSS из ESM: Vite должен обработать его и в тестах.
    server: { deps: { inline: ['@gravity-ui/uikit'] } },
    setupFiles: ['./src/test-setup.ts'],
    css: true,
  },
});

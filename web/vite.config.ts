import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

// Giao diện được process `api` phục vụ ở /ui/ từ thư mục nhúng internal/webui/dist.
// Bản build không được có script hay style nội tuyến: CSP của /ui/ chặn chúng
// (internal/webui/webui.go), và test Go kiểm index.html đã build.
export default defineConfig({
  base: '/ui/',
  plugins: [react()],
  resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
  build: {
    outDir: '../internal/webui/dist',
    // dist nằm ngoài thư mục gốc của Vite nên phải cho phép xoá tường minh. File
    // public/.gitkeep được chép lại sau mỗi lần build: thư mục nhúng không bao giờ rỗng.
    emptyOutDir: true,
    target: 'es2022',
    sourcemap: false,
    // Không nhúng asset thành data: URI vào CSS / JS; mọi thứ là file của chính origin.
    assetsInlineLimit: 0,
  },
  server: {
    port: 5173,
    strictPort: true,
    // `make ui-dev`: trang chạy ở http://localhost:5173/ui/, API và config.json lấy từ
    // process api cục bộ.
    proxy: {
      '/v1': 'http://localhost:8080',
      '/ui/config.json': 'http://localhost:8080',
    },
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.{ts,tsx}'],
    restoreMocks: true,
  },
});

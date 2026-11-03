import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const api = process.env.FOUNDRY_API ?? "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react()],
  server: { proxy: { "/v1": { target: api, changeOrigin: false } } },
  build: { outDir: "dist", sourcemap: false, target: "es2022", chunkSizeWarningLimit: 600 },
  worker: { format: "es" },
  test: { environment: "jsdom", include: ["src/**/*.test.{ts,tsx}"] },
});

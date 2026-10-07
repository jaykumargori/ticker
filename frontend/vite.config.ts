import { defineConfig } from "vite";
import solid from "vite-plugin-solid";

const backend = process.env.BACKEND ?? "http://localhost:8080";

export default defineConfig({
  plugins: [solid()],
  server: {
    port: 5173,
    proxy: {
      "/api": backend,
      "/ws": { target: backend.replace(/^http/, "ws"), ws: true },
    },
  },
  build: { target: "es2022" },
});

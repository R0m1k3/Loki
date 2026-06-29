import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// En dev, on proxifie /api vers le backend FastAPI (port 8080).
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": "http://localhost:8080",
    },
  },
});

import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import {defineConfig} from "vite";

export default defineConfig({
  base: "/admin/",
  plugins: [react(), tailwindcss()],
  build: {
    assetsInlineLimit: 0,
    emptyOutDir: true,
    outDir: "../internal/app/adminui/dist",
    sourcemap: false,
  },
});

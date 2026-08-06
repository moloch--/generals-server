import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import {defineConfig, type Plugin} from "vite";

const emitPublicIndex: Plugin = {
  name: "emit-public-index",
  enforce: "post",
  generateBundle(_options, bundle) {
    const html = bundle["public.html"];
    if (!html || html.type !== "asset") {
      throw new Error("The public frontend build did not emit public.html.");
    }
    html.fileName = "index.html";
  },
};

export default defineConfig({
  base: "/",
  plugins: [react(), tailwindcss(), emitPublicIndex],
  build: {
    assetsInlineLimit: 0,
    emptyOutDir: true,
    outDir: "../internal/app/publicui/dist",
    rollupOptions: {
      input: "public.html",
    },
    sourcemap: false,
  },
});

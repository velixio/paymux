import { writeFileSync } from "node:fs";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { fileURLToPath } from "node:url";
export default defineConfig({
  root: fileURLToPath(new URL(".", import.meta.url)),
  base: "/",
  plugins: [
    react(),
    tailwindcss(),
    {
      name: "go-embed-placeholder",
      apply: "build",
      writeBundle() {
        writeFileSync(new URL("dist/.keep", import.meta.url), "");
      },
    },
  ],
  resolve: {
    alias: { "@": fileURLToPath(new URL("../frontend", import.meta.url)) },
  },
  server: { port: 5174 },
  build: { outDir: "dist", assetsInlineLimit: 0, emptyOutDir: true },
});

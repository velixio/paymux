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
        writeFileSync(new URL("../../cmd/paymux-home/dist/.keep", import.meta.url), "");
      },
    },
  ],
  resolve: {
    alias: { "@": fileURLToPath(new URL("..", import.meta.url)) },
  },
  server: {
    port: 5174,
    // The guides are rendered from the repository's Markdown (docs/*.md).
    fs: { allow: [fileURLToPath(new URL("../..", import.meta.url))] },
  },
  // The Go website server embeds cmd/paymux-home/dist.
  build: { outDir: "../../cmd/paymux-home/dist", assetsInlineLimit: 0, emptyOutDir: true },
});

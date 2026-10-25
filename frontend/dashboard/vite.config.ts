import { readFileSync } from "node:fs";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { fileURLToPath } from "node:url";
export default defineConfig({
  root: fileURLToPath(new URL(".", import.meta.url)),
  base: "/assets/dashboard/",
  plugins: [
    {
      name: "paymux-development-shell",
      configureServer(server) {
        server.middlewares.use(async (req, res, next) => {
          if (
            req.method === "GET" &&
            req.headers["sec-fetch-mode"] === "navigate" &&
            ["/", "/assets/dashboard/"].includes(req.url || "")
          ) {
            res.statusCode = 302;
            res.setHeader("Location", "/dashboard/overview");
            res.end();
            return;
          }
          if (
            req.method === "GET" &&
            req.headers["sec-fetch-mode"] === "navigate" &&
            /^\/(dashboard(?:\/|$)|setup(?:\?|$))/.test(req.url || "")
          ) {
            try {
              const html = await server.transformIndexHtml(
                req.url || "/",
                readFileSync(new URL("index.html", import.meta.url), "utf8"),
              );
              res.setHeader("Content-Type", "text/html");
              res.end(html);
            } catch (error) {
              next(error);
            }
          } else next();
        });
      },
    },
    react(),
    tailwindcss(),
  ],
  resolve: {
    alias: { "@": fileURLToPath(new URL("..", import.meta.url)) },
  },
  server: {
    port: 5173,
    proxy: {
      "/dashboard": "http://localhost:18080",
      "/setup": "http://localhost:18080",
      "/assets/css": "http://localhost:18080",
      "/assets/favicon.svg": "http://localhost:18080",
      "/theme": "http://localhost:18080",
    },
  },
  build: {
    outDir: "../../web/static/dashboard",
    emptyOutDir: true,
    cssCodeSplit: false,
    rollupOptions: {
      input: fileURLToPath(new URL("src/main.tsx", import.meta.url)),
      output: {
        entryFileNames: "main.js",
        chunkFileNames: "[name]-[hash].js",
        assetFileNames: (asset) =>
          asset.names.some((n) => n.endsWith(".css"))
            ? "style.css"
            : "[name]-[hash][extname]",
      },
    },
  },
});

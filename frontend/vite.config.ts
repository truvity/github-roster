import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Built assets are embedded into the Go console and served under /app/.
// base must match that mount so hashed asset URLs resolve.
export default defineConfig({
  base: "/app/",
  plugins: [react()],
  build: { outDir: "dist", emptyOutDir: true },
});

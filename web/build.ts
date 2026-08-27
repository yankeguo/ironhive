// Bundles every entry in src/entries into a self-contained IIFE per entry in
// dist/, named "<name>-<content-hash>.<ext>" so Go can serve them with
// immutable caching and templates only need to match the entry name prefix.
// main.css is the site-wide stylesheet: Tailwind utilities scanned from the
// Go templates.
//
//   bun run build.ts          one-off production build (minified)
//   bun --watch run build.ts  dev loop (rebuilds on change, inline sourcemaps)

import { readdir, rm } from "node:fs/promises";
import tailwind from "bun-plugin-tailwind";

const dev = process.argv.includes("--watch") || !!process.env.BUN_WATCH;

const entryDir = "src/entries";
const entrypoints = (await readdir(entryDir))
  .filter((f) => f.endsWith(".ts") || f.endsWith(".css"))
  .sort()
  .map((f) => `${entryDir}/${f}`);

if (entrypoints.length === 0) {
  console.error(`no entries found in ${entryDir}`);
  process.exit(1);
}

// Drop stale hashed outputs from previous builds.
try {
  for (const f of await readdir("dist")) {
    if (f.endsWith(".js") || f.endsWith(".css") || f.endsWith(".map")) {
      await rm(`dist/${f}`);
    }
  }
} catch {
  // dist does not exist yet; Bun.build creates it.
}

const result = await Bun.build({
  entrypoints,
  outdir: "dist",
  naming: "[name]-[hash].[ext]",
  target: "browser",
  format: "iife",
  minify: !dev,
  sourcemap: dev ? "inline" : "none",
  plugins: [tailwind],
});

if (!result.success) {
  for (const msg of result.logs) console.error(msg);
  process.exit(1);
}
for (const out of result.outputs) console.log("built", out.path);

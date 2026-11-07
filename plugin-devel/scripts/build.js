#!/usr/bin/env node
"use strict";

// build.js - compile TypeScript runtime plugins to goja-compatible JS.
// Usage:
//   node scripts/build.js              (compile all)
//   node scripts/build.js podman       (compile one)
//   node scripts/build.js --deploy     (compile + copy to ../plugins/)
//   node scripts/build.js --check      (CI: verify plugins/ is up to date)
//   node scripts/build.js --watch      (watch mode)

const esbuild = require("esbuild");
const fs = require("fs");
const path = require("path");
const crypto = require("crypto");

const PLUGINS_SRC = path.resolve(__dirname, "../plugins");
const PLUGINS_OUT = path.resolve(__dirname, "../../plugins");
const BUILD_OUT = path.resolve(__dirname, "../dist");

const args = process.argv.slice(2);
const deploy = args.includes("--deploy");
const watch = args.includes("--watch");
const check = args.includes("--check");
const filter = args.find((a) => !a.startsWith("--"));

function discoverPlugins() {
  return fs
    .readdirSync(PLUGINS_SRC)
    .filter((name) => !filter || name === filter)
    .map((name) => ({ name, entry: path.join(PLUGINS_SRC, name, "plugin.ts") }))
    .filter((p) => fs.existsSync(p.entry));
}

function verifyOutput(file) {
  const content = fs.readFileSync(file, "utf8");
  if (!content.includes("module.exports")) {
    console.warn("WARNING: " + path.basename(file) + " has no module.exports - goja may not load it");
    return false;
  }
  return true;
}

function deployFiles(plugins) {
  fs.mkdirSync(PLUGINS_OUT, { recursive: true });
  for (const { name } of plugins) {
    const src = path.join(BUILD_OUT, name + ".js");
    const dst = path.join(PLUGINS_OUT, name + ".js");
    if (!fs.existsSync(src)) {
      console.error("MISSING: " + src);
      continue;
    }
    fs.copyFileSync(src, dst);
    console.log("  deployed " + name + ".js -> plugins/");
  }
}

function md5(buf) {
  return crypto.createHash("md5").update(buf).digest("hex");
}

function checkFiles(plugins) {
  let ok = true;
  for (const { name } of plugins) {
    const built = path.join(BUILD_OUT, name + ".js");
    const deployed = path.join(PLUGINS_OUT, name + ".js");
    if (!fs.existsSync(deployed)) {
      console.error("MISSING in plugins/: " + name + ".js");
      ok = false;
      continue;
    }
    if (md5(fs.readFileSync(built)) !== md5(fs.readFileSync(deployed))) {
      console.error("OUT OF DATE: plugins/" + name + ".js - run npm run deploy");
      ok = false;
    } else {
      console.log("  OK: " + name + ".js");
    }
  }
  return ok;
}

async function main() {
  const plugins = discoverPlugins();
  if (plugins.length === 0) {
    console.error("No plugins found" + (filter ? " matching: " + filter : ""));
    process.exit(1);
  }
  console.log("Building " + plugins.length + " plugin(s): " + plugins.map((p) => p.name).join(", "));
  fs.mkdirSync(BUILD_OUT, { recursive: true });

  const cfg = {
    entryPoints: Object.fromEntries(plugins.map((p) => [p.name, p.entry])),
    outdir: BUILD_OUT,
    bundle: false,
    platform: "node",
    format: "cjs",
    target: "es2015",
    sourcemap: false,
    minify: false,
    logLevel: "info",
  };

  if (watch) {
    const ctx = await esbuild.context(cfg);
    await ctx.watch();
    console.log("Watching...");
    return;
  }

  const result = await esbuild.build(cfg);
  if (result.errors.length > 0) process.exit(1);

  let allValid = true;
  for (const { name } of plugins) {
    if (!verifyOutput(path.join(BUILD_OUT, name + ".js"))) allValid = false;
  }

  if (check) process.exit(checkFiles(plugins) ? 0 : 1);
  if (deploy) deployFiles(plugins);

  console.log("\nDone. Output in plugin-devel/dist/");
  if (!deploy && !check) console.log("Run with --deploy to copy to plugins/");
  if (!allValid) process.exit(1);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});

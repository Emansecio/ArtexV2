#!/usr/bin/env node
"use strict";

// Patchright-backed Playwright MCP server for ArtexV2.
// Uses patchright-core's built-in MCP server directly so every browser session
// runs with Patchright's patched Chromium/CDP stack while preserving 100% of
// Playwright MCP's CLI flags (--headless, --browser, --proxy-server, etc.) and
// tool schemas (browser_navigate, browser_snapshot, browser_click, ...).
const { program } = require("patchright-core/lib/utilsBundle");
const { tools, libCli } = require("patchright-core/lib/coreBundle");
const pkg = require("patchright/package.json");

if (process.argv.includes("install-browser")) {
  const argv = process.argv.map((arg) => (arg === "install-browser" ? "install" : arg));
  libCli.decorateProgram(program);
  void program.parseAsync(argv);
} else {
  const p = program.version("Patchright MCP " + pkg.version).name("Patchright MCP");
  tools.decorateMCPCommand(p, pkg.version);
  void program.parseAsync(process.argv);
}

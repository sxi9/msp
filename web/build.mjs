// Bundles src/main.tsx into dist/app.js (+ dist/app.css from the imported
// styles.css). dist/index.html is committed by WP0 and never rewritten here.
//
// The @xterm/xterm stylesheet needed by the remote shell terminal (RS1) rides
// into dist/app.css with no change to this file: src/styles.css begins with
// `@import '@xterm/xterm/css/xterm.css';`, which esbuild resolves from
// node_modules and concatenates into the single app.css (see
// docs/specs/facts.json, esbuild_css). bundle:true + the .css loader below are
// what make that work.
import * as esbuild from 'esbuild';

await esbuild.build({
  entryPoints: ['src/main.tsx'],
  bundle: true,
  minify: true,
  format: 'esm',
  target: 'es2020',
  jsx: 'automatic',
  jsxImportSource: 'preact',
  outfile: 'dist/app.js',
  loader: { '.css': 'css' },
  logLevel: 'info',
});

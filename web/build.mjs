// Bundles src/main.tsx into dist/app.js (+ dist/app.css from the imported
// styles.css). dist/index.html is committed by WP0 and never rewritten here.
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

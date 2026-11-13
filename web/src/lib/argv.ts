// Command console helpers: one argument per line in, shell-quoted preview out.

/** One argument per line, trimmed; empty lines dropped. Quotes are kept literally. */
export function parseArgv(text: string): string[] {
  return text.split(/\r?\n/).map((l) => l.trim()).filter((l) => l.length > 0);
}

const SAFE = /^[A-Za-z0-9_\-./=:@%+,]+$/;

/** POSIX-style quoted display so the operator sees exactly which argv is sent. */
export function argvPreview(argv: string[]): string {
  return argv.map((a) => (SAFE.test(a) ? a : `'${a.replace(/'/g, "'\\''")}'`)).join(' ');
}

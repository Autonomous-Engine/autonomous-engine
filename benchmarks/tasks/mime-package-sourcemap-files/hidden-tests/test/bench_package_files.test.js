import assert from 'node:assert';
import child_process from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';

// Held-out acceptance tests: the published package is self-contained. Run after `tsc`, so
// dist/ exists; `npm pack --dry-run` lists exactly what would be published.

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

function packedFiles() {
  const out = child_process.execSync('npm pack --dry-run --json --ignore-scripts', {
    cwd: root,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'ignore'],
  });
  return new Set(JSON.parse(out)[0].files.map((f) => f.path.replace(/\\/g, '/')));
}

describe('bench: published package contents', () => {
  const files = packedFiles();

  it('ships every file named in package.json#exports, main and bin', () => {
    const pkg = JSON.parse(fs.readFileSync(path.join(root, 'package.json'), 'utf8'));
    const targets = [pkg.main, ...Object.values(pkg.exports), ...Object.values(pkg.bin ?? {})];
    for (const target of targets) {
      const rel = path.posix.normalize(target);
      assert.ok(files.has(rel), `${rel} is referenced by package.json but not published`);
    }
  });

  it('ships the sources that every published source map points to', () => {
    const maps = [...files].filter((f) => f.endsWith('.js.map'));
    assert.ok(maps.length > 0, 'expected the build to publish source maps');
    const missing = [];
    for (const map of maps) {
      const json = JSON.parse(fs.readFileSync(path.join(root, map), 'utf8'));
      for (const source of json.sources) {
        const resolved = path.posix.normalize(path.posix.join(path.posix.dirname(map), json.sourceRoot || '', source));
        if (!files.has(resolved)) missing.push(`${map} -> ${resolved}`);
      }
    }
    assert.deepStrictEqual(missing, []);
  });

  it('ships both the library sources and the type tables', () => {
    for (const f of ['src/index.ts', 'src/index_lite.ts', 'src/Mime.ts', 'types/standard.ts', 'types/other.ts']) {
      assert.ok(files.has(f), `${f} is not published`);
    }
  });

  it('does not publish tests, scripts or build tooling', () => {
    for (const f of files) {
      assert.ok(!f.startsWith('test/'), `${f} should not be published`);
      assert.ok(!f.startsWith('node_modules/'), `${f} should not be published`);
      assert.ok(!f.startsWith('dist/scripts/') && !f.startsWith('scripts/'), `${f} should not be published`);
      assert.ok(f !== 'tsconfig.json', `${f} should not be published`);
    }
  });
});

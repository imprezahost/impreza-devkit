import test from 'node:test';
import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import {expectedFiles, readURL, verifyRelease} from './verify-cli-release.mjs';

const tag = 'cli-go-v0.2.2';
const digest = data => createHash('sha256').update(data).digest('hex');
function fixture() {
  const files = expectedFiles(tag), data = new Map(files.archives.map(name => [name, Buffer.from(name)]));
  data.set(files.checksum, Buffer.from(files.archives.map(name => `${digest(data.get(name))}  ${name}\n`).join('')));
  const release = {tag_name: tag, draft: false, prerelease: false, assets: []};
  function refresh() {
    release.assets = [...data].map(([name, bytes]) => ({name, state: 'uploaded', size: bytes.length,
      digest: `sha256:${digest(bytes)}`, browser_download_url: `https://github.com/imprezahost/impreza-devkit/releases/download/${tag}/${name}`}));
  }
  refresh();
  const calls = [];
  async function fetchFn(url, options) {
    calls.push({url, options});
    assert(!options.method || options.method === 'GET', 'Verifier must never mutate a release');
    return new Response(url.startsWith('https://api.github.com/') ? JSON.stringify(release) : data.get(url.split('/').at(-1)));
  }
  return {files, data, release, refresh, calls, fetchFn};
}

test('existing complete release is verified repeatedly without writes', async () => {
  const f = fixture();
  for (let n = 0; n < 2; n++) assert.deepEqual(await verifyRelease(tag, {...f, token: 'test-only'}), {tag, verified_assets: 6});
  assert.equal(f.calls.length, 14);
  for (const c of f.calls) assert.equal(c.options.headers.Authorization, c.url.startsWith('https://api.github.com/') ? 'Bearer test-only' : undefined);
});

for (const [name, mutate] of [
  ['incomplete release', f => f.release.assets.pop()],
  ['duplicate asset', f => f.release.assets.push(f.release.assets[0])],
  ['draft release', f => f.release.draft = true],
  ['different tag', f => f.release.tag_name = 'cli-go-v0.2.3'],
  ['asset still uploading', f => f.release.assets[0].state = 'starter'],
  ['untrusted asset URL', f => f.release.assets[0].browser_download_url = 'https://example.org/payload'],
  ['digest mismatch', f => f.release.assets[0].digest = `sha256:${'0'.repeat(64)}`],
  ['missing digest', f => delete f.release.assets[0].digest],
  ['corrupt archive with updated metadata', f => {f.data.set(f.files.archives[0], Buffer.from('corrupt')); f.refresh();}],
  ['duplicate checksum', f => {f.data.set(f.files.checksum, Buffer.from(`${digest(f.data.get(f.files.archives[0]))}  ${f.files.archives[0]}\n`.repeat(5))); f.refresh();}],
]) test(`refuses ${name}`, async () => {
  const f = fixture(); mutate(f); await assert.rejects(verifyRelease(tag, f));
});

test('missing release fails without attempting to create it', async () => {
  let count = 0;
  await assert.rejects(verifyRelease(tag, {fetchFn: async () => {count++; return new Response('', {status: 404});}}), /HTTP 404/);
  assert.equal(count, 1);
});
test('invalid tag is rejected before network access', async () => {
  for (const value of ['', undefined, 'cli-go-v01.2.3', '../main', 'cli-go-v1.2.3\n', 'sdk-v1.2.3']) {
    let calls = 0;
    await assert.rejects(verifyRelease(value, {fetchFn: () => { calls++; throw new Error('unexpected network'); }}), /Invalid CLI release tag/);
    assert.equal(calls, 0);
  }
});
test('trusted asset redirect never carries API credentials', async () => {
  let count = 0;
  const data = await readURL('https://github.com/example', {token: 'test-only', limit: 10, fetchFn: async (_, options) => {
    assert.equal(options.headers.Authorization, undefined);
    return ++count === 1 ? new Response('', {status: 302, headers: {location: 'https://release-assets.githubusercontent.com/example'}}) : new Response('ok');
  }});
  assert.equal(data.toString(), 'ok'); assert.equal(count, 2);
});
test('redirects to untrusted hosts and API redirects fail closed', async () => {
  for (const api of [false, true]) {
    let calls = 0;
    await assert.rejects(readURL(api ? 'https://api.github.com/example' : 'https://github.com/example', {api, limit: 10, fetchFn: async () => {
      calls++; return new Response('', {status: 302, headers: {location: 'https://example.org/collect'}});
    }}));
    assert.equal(calls, 1);
  }
});
test('oversized streaming response is refused', async () => {
  await assert.rejects(readURL('https://github.com/example', {limit: 2, fetchFn: async () => new Response('large')}), /size limit/);
});

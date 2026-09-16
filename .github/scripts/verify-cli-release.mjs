import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import {pathToFileURL} from 'node:url';

const repository = 'imprezahost/impreza-devkit';
const sha256 = data => createHash('sha256').update(data).digest('hex');

export function expectedFiles(tag) {
  assert(/^cli-go-v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(tag), 'Invalid CLI release tag');
  const prefix = `impreza-cli-go_${tag.slice(8)}_`;
  return {
    checksum: `${prefix}checksums.txt`,
    archives: ['Darwin_arm64.tar.gz', 'Darwin_x86_64.tar.gz', 'Linux_arm64.tar.gz',
      'Linux_x86_64.tar.gz', 'Windows_x86_64.zip'].map(name => prefix + name),
  };
}

export async function readURL(url, {api = false, token = '', limit, fetchFn = fetch} = {}) {
  for (let redirects = 0; redirects <= 5; redirects++) {
    const u = new URL(url);
    const allowed = api ? ['api.github.com'] : ['github.com', 'release-assets.githubusercontent.com'];
    assert(u.protocol === 'https:' && !u.username && !u.password && !u.port && !u.hash && allowed.includes(u.hostname), 'Untrusted download destination');
    const headers = {'User-Agent': 'impreza-cli-release-verifier'};
    if (api && token) headers.Authorization = `Bearer ${token}`;
    const response = await fetchFn(url, {headers, redirect: 'manual', signal: AbortSignal.timeout(60_000)});
    if ([301, 302, 303, 307, 308].includes(response.status)) {
      await response.body?.cancel();
      assert(!api && redirects < 5, 'Unexpected API redirect or redirect limit exceeded');
      const location = response.headers.get('location');
      assert(location, 'Redirect has no destination');
      url = new URL(location, url).href;
      continue;
    }
    assert(response.status === 200, `Download failed with HTTP ${response.status}`);
    const reader = response.body.getReader();
    const chunks = []; let length = 0;
    for (;;) {
      const {done, value} = await reader.read();
      if (done) break;
      length += value.length;
      if (length > limit) { await reader.cancel(); throw new Error('Download exceeds size limit'); }
      chunks.push(value);
    }
    return Buffer.concat(chunks);
  }
}

export async function verifyRelease(tag, {fetchFn = fetch, token = ''} = {}) {
  const files = expectedFiles(tag);
  const expected = [files.checksum, ...files.archives];
  const bytes = await readURL(`https://api.github.com/repos/${repository}/releases/tags/${tag}`,
    {api: true, token, limit: 2 * 1024 * 1024, fetchFn});
  const release = JSON.parse(bytes);
  assert(release.tag_name === tag && release.draft === false && release.prerelease === false, 'Release must be published and stable');
  assert(Array.isArray(release.assets), 'Missing release assets');
  assert.deepEqual(release.assets.map(a => a.name).sort(), [...expected].sort(), 'Incomplete, duplicate or unexpected release assets');
  const assets = new Map(release.assets.map(a => [a.name, a]));
  async function download(name, limit) {
    const asset = assets.get(name);
    const url = `https://github.com/${repository}/releases/download/${tag}/${name}`;
    assert(asset.state === 'uploaded' && asset.browser_download_url === url, 'Asset is not ready or has an unexpected URL');
    assert(Number.isSafeInteger(asset.size) && asset.size > 0 && asset.size <= limit, 'Invalid asset size');
    assert(/^sha256:[a-f0-9]{64}$/.test(asset.digest), 'Missing asset digest');
    const data = await readURL(url, {limit, fetchFn});
    assert(data.length === asset.size && `sha256:${sha256(data)}` === asset.digest, 'Asset digest or size mismatch');
    return data;
  }
  const text = (await download(files.checksum, 16 * 1024)).toString('utf8');
  const checksums = new Map();
  for (const line of text.trim().split(/\r?\n/)) {
    const match = /^([a-f0-9]{64}) [ *]([^\s]+)$/.exec(line);
    assert(match && files.archives.includes(match[2]) && !checksums.has(match[2]), 'Invalid or duplicate checksum entry');
    checksums.set(match[2], match[1]);
  }
  assert(checksums.size === files.archives.length, 'Incomplete checksums');
  for (const name of files.archives) {
    assert(sha256(await download(name, 32 * 1024 * 1024)) === checksums.get(name), 'Archive checksum mismatch');
  }
  return {tag, verified_assets: expected.length};
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try { console.log(JSON.stringify(await verifyRelease(process.env.RELEASE_TAG, {token: process.env.GITHUB_TOKEN}))); }
  catch (error) { console.error(error.message); process.exitCode = 1; }
}

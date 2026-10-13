const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const vm = require('node:vm');

async function runPopup(tabURL) {
  let click, reads = 0, request;
  const message = {};
  const button = { addEventListener: (_, handler) => { click = handler; } };
  vm.runInNewContext(readFileSync('youtube-extension/connect.js', 'utf8'), {
    URL, AbortSignal, navigator: { language: 'ru' },
    document: { documentElement: {}, getElementById: id => id === 'connect' ? button : message },
    chrome: {
      tabs: { query: async () => [{ url: tabURL }] },
      cookies: { getAll: async ({ domain }) => {
        assert.equal(domain, 'youtube.com');
        reads++;
        return [
          { domain: '.youtube.com', name: 'SAPISID', value: 'dummy', path: '/', secure: true },
          { domain: '.google.com', name: 'unrelated', value: 'must-not-send' }
        ];
      } }
    },
    fetch: async (url, options) => { request = { url, options }; return { ok: true }; }
  });
  await click();
  return { reads, request, message, button };
}

test('connection sends only YouTube session to loopback after a click', async () => {
  const result = await runPopup(`http://127.0.0.1:12345/youtube-connect#${'a'.repeat(64)}`);
  assert.equal(result.reads, 1);
  assert.equal(result.request.url, 'http://127.0.0.1:12345/youtube-session');
  assert.equal(result.request.options.redirect, 'error');
  assert.equal(result.request.options.credentials, 'omit');
  assert.equal(JSON.parse(result.request.options.body).length, 1);
  assert.equal(result.request.options.headers.Authorization, `Bearer ${'a'.repeat(64)}`);
  assert.match(result.message.textContent, /Подключено/);
});

test('unrelated pages cannot cause session reads or transmission', async () => {
  for (const target of [
    'https://evil.test/youtube-connect',
    `http://127.0.0.1:12345/youtube-connect#short`,
    `http://127.0.0.1:12345/other#${'a'.repeat(64)}`,
    `http://username@127.0.0.1:12345/youtube-connect#${'a'.repeat(64)}`
  ]) {
    const result = await runPopup(target);
    assert.equal(result.reads, 0);
    assert.equal(result.request, undefined);
    assert.equal(result.button.disabled, false);
  }
});

import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const script = readFileSync(new URL('../web/cpamc-session-bridge.js', import.meta.url), 'utf8');
function fixture() {
  const replies = [], calls = [];
  const source = {postMessage: (reply, origin) => replies.push({reply, origin})};
  const frame = {src: 'https://panel.test/v0/resource/plugins/quota-guard/index', contentWindow: source};
  const session = {isAuthenticated: true, connectionStatus: 'connected', apiBase: 'https://panel.test'};
  const listeners = {};
  let observer, revision = 1;
  const client = {getConnectionRevision: () => revision, instance: {request: async args => {
    calls.push(args); return {status: 200, data: {policy: {mode: 'observe'}, accounts: []}};
  }}};
  vm.runInNewContext(script, {URL, location: {origin: 'https://panel.test', href: 'https://panel.test/management.html'},
    document: {querySelectorAll: () => [frame], documentElement: {getAttribute: () => 'dark'}},
    getComputedStyle: () => ({getPropertyValue: name => name === '--bg-primary' ? '#111111' : name === '--dur-press' ? '.16s' : '',
      fontFamily: 'CPAMC fixture font', fontSize: '16px', lineHeight: '24px'}),
    MutationObserver: class {constructor(callback) {observer = callback;} observe() {}},
    window: {innerWidth:1280, addEventListener: (name, callback) => {listeners[name] = callback;}},
    Fm: {getState: () => session}, G: client});
  const send = (data = {}, event = {}) => listeners.message({source, origin: 'https://panel.test',
    data: {channel: 'quota-guard:cpamc-session:v1', type: 'request', id: 'test-request', operation: 'status', ...data}, ...event});
  return {send, replies, calls, session, client, frame, source, themeChange: () => observer(), resize: () => listeners.resize(), advance: () => {revision++;}};
}
test('uses existing authenticated API client and only returns plugin data', async () => {
  const f = fixture(); await f.send();
  assert.equal(f.calls[0].url, 'https://panel.test/v0/management/plugins/quota-guard/status');
  assert.equal(f.calls[0].method, 'GET');
  assert.equal(f.calls[0].headers, undefined);
  assert.equal(f.replies[0].origin, 'https://panel.test');
  assert.equal(f.replies[0].reply.status, 200);
  assert.deepEqual(Object.keys(f.replies[0].reply.data).sort(), ['accounts', 'policy']);
});
test('saving policy uses PUT and refresh uses POST', async () => {
  const f = fixture(); const policy = {revision: 2, mode: 'observe'};
  await f.send({operation: 'policy', body: policy});
  await f.send({operation: 'refresh'});
  assert.equal(f.calls[0].method, 'PUT'); assert.equal(f.calls[0].data, policy);
  assert.equal(f.calls[1].method, 'POST'); assert.equal(f.calls[1].data, undefined);
});
test('rejects other origins, frames and channels without invoking API', async () => {
  const f = fixture();
  await f.send({}, {origin: 'https://other.test'});
  await f.send({}, {source: {postMessage() {}}});
  await f.send({channel: 'other'});
  f.frame.src = 'https://panel.test/v0/resource/plugins/other/index';
  await f.send();
  assert.equal(f.calls.length, 0); assert.equal(f.replies.length, 0);
});
test('rejects detached iframe and frames hosted by other origins', async () => {
  const f = fixture(); f.frame.src = 'https://other.test/v0/resource/plugins/quota-guard/index';
  await f.send(); assert.equal(f.calls.length, 0);
});
test('logged out and disconnected sessions cannot read or save', async () => {
  const f = fixture(); f.session.isAuthenticated = false;
  await f.send(); await f.send({operation: 'policy', body: {}});
  f.session.isAuthenticated = true; f.session.connectionStatus = 'disconnected';
  await f.send();
  assert.equal(f.calls.length, 0); assert.ok(f.replies.every(r => r.reply.status === 401));
});
test('arbitrary URLs, prototype keys and invalid policy bodies are rejected', async () => {
  const f = fixture();
  for (const operation of ['../config', 'https://other.test', 'constructor', 'toString']) await f.send({operation});
  for (const body of [undefined, null, [], 'bad', {large: 'x'.repeat(1024 * 1024)}]) await f.send({operation: 'policy', body});
  assert.equal(f.calls.length, 0); assert.ok(f.replies.every(r => r.reply.status === 400));
});
test('cross-server CPAMC connections cannot use this same-origin bridge', async () => {
  const f = fixture(); f.session.apiBase = 'https://other.test'; await f.send();
  assert.equal(f.calls.length, 0); assert.equal(f.replies[0].reply.status, 400);
});
test('logout or server switch during a request withholds account data', async () => {
  for (const change of [f => {f.session.isAuthenticated = false;}, f => f.advance()]) {
    const f = fixture();
    f.client.instance.request = async () => {change(f); return {status: 200, data: {accounts: ['private']}};};
    await f.send();
    assert.equal(f.replies[0].reply.status, 401); assert.equal(f.replies[0].reply.data.accounts, undefined);
  }
});
test('backend 401 is propagated and does not expose the management key', async () => {
  const f = fixture();
  f.client.instance.request = async () => {throw Object.assign(new Error('internal detail'), {status: 401});};
  await f.send();
  assert.equal(f.replies[0].reply.status, 401);
  assert.match(f.replies[0].reply.data.error, /重新登录/);
});
test('plugin HTML has no second login form, password field, key storage or direct authenticated fetch', () => {
  const page = readFileSync(new URL('../web/index.html', import.meta.url), 'utf8') + readFileSync(new URL('../web/app.js', import.meta.url), 'utf8');
  assert.doesNotMatch(page, /id="login|type="password"|localStorage|sessionStorage|Authorization|fetch\(/);
  assert.match(page, /window\.parent\.postMessage/);
});

test('theme inherits CPAMC colors and propagates changes without an API request', async () => {
  const f = fixture(); await f.send({operation:'appearance'});
  assert.equal(f.calls.length, 0); assert.equal(f.replies[0].reply.data.theme, 'dark');
  assert.equal(f.replies[0].reply.data.colors['--bg-primary'], '#111111');
  assert.equal(f.replies[0].reply.data.colors['--dur-press'], '.16s');
  assert.equal(f.replies[0].reply.data.typography.fontFamily, 'CPAMC fixture font');
  assert.equal(f.replies[0].reply.data.typography.lineHeight, '24px');
  assert.equal(f.replies[0].reply.data.layout.inline, '38.4px');
  assert.equal(f.replies[0].reply.data.layout.top, '70px');
  f.themeChange(); assert.equal(f.replies[1].reply.type, 'appearance');
  f.resize(); assert.equal(f.replies[2].reply.type, 'appearance');
  f.session.isAuthenticated = false; f.themeChange(); f.resize(); assert.equal(f.replies.length, 3);
});

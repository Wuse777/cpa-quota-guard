// Runs inside the CPAMC 1.26.5 module, with its existing API client and auth store.
// Requests are limited to this plugin's attached, same-origin iframe. No key is
// copied into the iframe, browser storage, URLs, or messages.
(() => {
  'use strict';
  const channel = 'quota-guard:cpamc-session:v1';
  const resource = '/v0/resource/plugins/quota-guard/index';
  const operations = {status: 'GET', policy: 'PUT', refresh: 'POST'};
  const themeVars = ['--bg-primary','--bg-secondary','--bg-tertiary','--bg-hover','--text-primary','--text-secondary',
    '--text-tertiary','--border-color','--primary-color','--primary-hover','--primary-contrast','--success-color',
    '--warning-color','--error-color','--shadow','--shadow-lg','--floating-surface','--floating-shadow',
    '--border-hover','--primary-active','--viz-success','--viz-failure','--quota-medium-color',
    '--success-badge-bg','--success-badge-text','--success-badge-border',
    '--failure-badge-bg','--failure-badge-text','--failure-badge-border',
    '--ease-out-strong','--dur-press','--dur-hover'];
  function appearance() {
    const style = getComputedStyle(document.documentElement);
    const body = getComputedStyle(document.body);
    const mobile = window.innerWidth <= 768;
    return {theme: document.documentElement.getAttribute('data-theme') || 'light',
      colors: Object.fromEntries(themeVars.map(name => [name, style.getPropertyValue(name).trim()])),
      typography: {fontFamily: body.fontFamily, fontSize: body.fontSize, lineHeight: body.lineHeight},
      layout: {inline: mobile ? '16px' : `${Math.min(48, Math.max(20, window.innerWidth * .03))}px`,
        top: mobile ? `calc(${style.getPropertyValue('--header-height').trim() || '74px'} + 16px)` : '70px',
        bottom: mobile ? '24px' : '40px'}};
  }
  function attached(source) {
    return Array.from(document.querySelectorAll('iframe')).some(frame => {
      try {
        const url = new URL(frame.src, location.href);
        return frame.contentWindow === source && url.origin === location.origin && url.pathname === resource;
      } catch { return false; }
    });
  }
  window.addEventListener('message', async event => {
    const request = event.data;
    if (event.origin !== location.origin || !attached(event.source) || !request ||
        request.channel !== channel || request.type !== 'request' ||
        typeof request.id !== 'string' || request.id.length > 100) return;
    const respond = (status, data) => {
      if (attached(event.source)) event.source.postMessage({channel, type: 'response', id: request.id, status, data}, location.origin);
    };
    const session = Fm.getState();
    if (!session.isAuthenticated || session.connectionStatus !== 'connected') {
      respond(401, {error: '请先登录 CPAMC 管理面板。'});
      return;
    }
    if (request.operation === 'appearance') { respond(200, appearance()); return; }
    if (!Object.hasOwn(operations, request.operation)) {
      respond(400, {error: '不支持的插件操作。'});
      return;
    }
    const url = new URL('/v0/management/plugins/quota-guard/' + request.operation, session.apiBase);
    if (url.origin !== location.origin) {
      respond(400, {error: '请在 CPA 服务器的 CPAMC 管理面板中使用此插件。'});
      return;
    }
    if (request.operation === 'policy' && (!request.body || Array.isArray(request.body) ||
        typeof request.body !== 'object' || JSON.stringify(request.body).length > 1024 * 1024)) {
      respond(400, {error: '策略格式无效或过大。'});
      return;
    }
    const revision = G.getConnectionRevision();
    try {
      const response = await G.instance.request({url: url.href, method: operations[request.operation],
        data: request.operation === 'policy' ? request.body : undefined, timeout: 20000});
      if (!Fm.getState().isAuthenticated || revision !== G.getConnectionRevision()) {
        respond(401, {error: 'CPAMC 登录会话已结束，请重新登录。'});
        return;
      }
      respond(response.status, response.data);
    } catch (error) {
      const status = error.status || error.response?.status || 502;
      respond(status, {error: status === 401 ? 'CPAMC 登录已失效，请重新登录。' :
        (typeof error.message === 'string' ? error.message : '插件请求失败。')});
    }
  });
  function syncAppearance() {
    if (!Fm.getState().isAuthenticated) return;
    for (const frame of document.querySelectorAll('iframe')) {
      if (attached(frame.contentWindow)) frame.contentWindow.postMessage({channel, type:'appearance', data:appearance()}, location.origin);
    }
  }
  window.addEventListener('resize', syncAppearance);
  if (typeof MutationObserver !== 'undefined') new MutationObserver(syncAppearance)
    .observe(document.documentElement, {attributes:true, attributeFilter:['data-theme','class','style']});
})();

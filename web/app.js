(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const channel = 'quota-guard:cpamc-session:v1';
  const pending = new Map();
  const copy = value => JSON.parse(JSON.stringify(value));
  const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const validDate = value => value && !String(value).startsWith('0001-') && Number.isFinite(Date.parse(value));
  const date = value => validDate(value) ? new Date(value).toLocaleString('zh-CN', {hour12:false}) : '尚未查询';
  const remaining = value => {
    if (!validDate(value)) return '—';
    const seconds = Math.ceil((Date.parse(value) - Date.now()) / 1000);
    if (seconds <= 0) return '已进入使用窗口';
    const hours = Math.floor(seconds / 3600), minutes = Math.floor(seconds % 3600 / 60);
    return hours >= 24 ? `${Math.floor(hours / 24)} 天 ${hours % 24} 小时` : `${hours} 小时 ${minutes} 分`;
  };
  let state, draft, dirty = false, connected = false, loading = false, serial = 0;
  const reducedMotion = () => window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  function busy(id, value) {
    const button = $(id); button.disabled = value; button.classList.toggle('is-loading', value);
    button.setAttribute('aria-busy', String(value));
  }
  // Patch live nodes so polling preserves focus and quota meters can transition.
  function markup(container, html) {
    const template = document.createElement('template'); template.innerHTML = html;
    function patch(parent, desired) {
      for (let i = 0; i < desired.childNodes.length; i++) {
        const next = desired.childNodes[i], current = parent.childNodes[i];
        if (!current) { parent.append(next.cloneNode(true)); continue; }
        if (current.nodeType !== next.nodeType || current.nodeName !== next.nodeName ||
            current.nodeType === 1 && current.getAttribute('data-key') !== next.getAttribute('data-key')) {
          current.replaceWith(next.cloneNode(true)); continue;
        }
        if (next.nodeType !== 1) { if (current.nodeValue !== next.nodeValue) current.nodeValue = next.nodeValue; continue; }
        for (const attr of [...current.attributes]) if (!next.hasAttribute(attr.name)) current.removeAttribute(attr.name);
        for (const attr of next.attributes) if (current.getAttribute(attr.name) !== attr.value) current.setAttribute(attr.name, attr.value);
        if (current instanceof HTMLInputElement) {
          if (current.value !== next.value) current.value = next.value;
          current.checked = next.checked;
        }
        patch(current, next);
      }
      while (parent.childNodes.length > desired.childNodes.length) parent.lastChild.remove();
    }
    patch(container, template.content);
  }
  function reveal() {
    if (reducedMotion()) return;
    [...document.querySelectorAll('header,.stat,.upcoming,.tabs,.view:not([hidden]) .section-heading,.view:not([hidden]) .account')].forEach((node, i) => {
      node.style.setProperty('--reveal-delay', `${Math.min(i * .07, .36)}s`);
      node.classList.add('reveal');
    });
  }
  document.addEventListener('animationend', event => {
    if (event.animationName === 'reveal') { event.target.classList.remove('reveal'); event.target.style.removeProperty('--reveal-delay'); }
    if (event.animationName === 'page-enter') event.target.classList.remove('is-entering');
  });
  if (window.parent !== window) document.documentElement.classList.add('embedded');
  function message(text = '', error = false) { $('message').textContent = text; $('message').classList.toggle('error', error); }
  function setDirty() { dirty = true; $('dirty').textContent = '有未保存的修改'; $('save').disabled = false; }
  function disconnect(text) { connected = false; $('session').hidden = false; $('session').querySelector('h2').textContent = '连接管理面板后继续'; $('session').querySelector('p').textContent = text; $('content').hidden = true; $('toolbar').hidden = true; }
  function request(operation, body) {
    return new Promise((resolve, reject) => {
      if (window.parent === window) { reject(new Error('请在 CPAMC 的「账号额度保护」页面使用插件。')); return; }
      const id = `qg-${Date.now()}-${++serial}`;
      const timer = setTimeout(() => { pending.delete(id); reject(new Error('管理面板连接超时，请刷新 CPAMC 页面后重试。')); }, 25000);
      pending.set(id, {resolve, reject, timer});
      window.parent.postMessage({channel, type:'request', id, operation, body}, location.origin);
    });
  }
  function appearance(data) {
    if (!data || !data.colors) return;
    const vars = ['--bg-primary','--bg-secondary','--bg-tertiary','--bg-hover','--text-primary','--text-secondary','--text-tertiary','--border-color','--primary-color','--primary-hover','--primary-contrast','--success-color','--warning-color','--error-color','--shadow','--shadow-lg',
      '--border-hover','--primary-active','--viz-success','--viz-failure','--quota-medium-color',
      '--success-badge-bg','--success-badge-text','--success-badge-border','--failure-badge-bg','--failure-badge-text','--failure-badge-border','--ease-out-strong','--dur-press','--dur-hover'];
    for (const key of vars) if (typeof data.colors[key] === 'string' && data.colors[key]) document.documentElement.style.setProperty(key, data.colors[key]);
    for (const [field, variable] of Object.entries({fontFamily:'--cpamc-font-family',fontSize:'--cpamc-font-size',lineHeight:'--cpamc-line-height'})) {
      if (typeof data.typography?.[field] === 'string' && data.typography[field]) document.documentElement.style.setProperty(variable, data.typography[field]);
    }
    for (const [field, variable] of Object.entries({inline:'--cpamc-page-padding-inline',top:'--cpamc-page-padding-top',bottom:'--cpamc-page-padding-bottom'})) {
      if (typeof data.layout?.[field] === 'string' && data.layout[field]) document.documentElement.style.setProperty(variable, data.layout[field]);
    }
    document.documentElement.style.colorScheme = data.theme === 'dark' ? 'dark' : 'light';
    document.documentElement.dataset.theme = data.theme;
  }
  window.addEventListener('message', event => {
    if (event.source !== window.parent || event.origin !== location.origin || event.data?.channel !== channel) return;
    const reply = event.data;
    if (reply.type === 'appearance') { appearance(reply.data); return; }
    if (reply.type !== 'response' || !pending.has(reply.id)) return;
    const task = pending.get(reply.id); pending.delete(reply.id); clearTimeout(task.timer);
    if (reply.status >= 200 && reply.status < 300) task.resolve(reply.data);
    else { const error = new Error(reply.data?.error || '插件请求失败'); error.status = reply.status; if (reply.status === 401) disconnect(error.message); task.reject(error); }
  });
  function ruleFor(account, create = false) {
    let rule = draft.rules.find(r => [account.name, account.id, account.auth_index].includes(r.account));
    if (!rule && create) { rule = {account:account.name || account.id, enabled:false, min_remaining_percent:20}; draft.rules.push(rule); }
    return rule;
  }
  function autoKey(account) { return [account.name, account.id, account.auth_index].find(key => Object.hasOwn(draft.auto_reset.accounts, key)) || account.name || account.id; }
  function accountAuto(account) { const key = autoKey(account); return !Object.hasOwn(draft.auto_reset.accounts, key) || draft.auto_reset.accounts[key]; }
  function countdowns() { document.querySelectorAll('[data-countdown]').forEach(node => { node.textContent = remaining(node.dataset.countdown); }); }
  function render() {
    const accounts = state.accounts || [], policy = state.policy;
    $('mode-label').textContent = policy.mode === 'enforce' ? '执行保护' : '仅观察';
    $('account-count').textContent = accounts.length; $('account-tab-count').textContent = accounts.length;
    $('held-count').textContent = accounts.filter(a => a.would_block).length;
    const credits = accounts.flatMap(a => (a.reset?.credits || []).filter(c => Date.parse(c.expires_at) > Date.now()));
    $('credit-count').textContent = credits.length;
    $('auto-label').textContent = policy.auto_reset.enabled ? `已开启 · 到期前 ${policy.auto_reset.before_hours} 小时` : '自动使用已关闭';
    $('auto-enabled').checked = draft.auto_reset.enabled; $('auto-hours').value = draft.auto_reset.before_hours;
    document.querySelectorAll('[data-field]').forEach(input => { input.value = draft[input.dataset.field]; });
    const scheduled = accounts.filter(a => a.reset?.enabled && !a.account.disabled && !a.reset.error && validDate(a.reset.next_at) && a.reset.credits?.some(c => Date.parse(c.expires_at) > Date.now())).sort((a,b) => Date.parse(a.reset.next_at) - Date.parse(b.reset.next_at));
    const next = scheduled[0];
    $('next-title').textContent = next ? '下一张临期卡，已安排自动使用' : policy.auto_reset.enabled ? '后台持续检查重置卡' : '自动使用已关闭';
    $('next-description').textContent = next ? `${next.account.name} · ${next.reset.message}。关闭面板仍会运行。` : '在策略设置中管理总开关，在账号卡片中单独启用或关闭。';
    $('next-state').textContent = state.reset_error ? '自动使用已暂停' : next ? '已安排' : policy.auto_reset.enabled ? '检查中' : '未启用';
    $('next-state').className = 'badge' + (state.reset_error ? ' warn' : next ? ' good' : '');
    $('next-countdown').textContent = next ? remaining(next.reset.next_at) : '—';
    if (next) $('next-countdown').dataset.countdown = next.reset.next_at; else delete $('next-countdown').dataset.countdown;
    $('next-date').textContent = next ? `计划时间 ${date(next.reset.next_at)}` : '按浏览器本地时区显示时间';
    const times = accounts.map(a => a.attempt_at).filter(validDate).sort();
    $('updated-at').textContent = times.length ? `最近检查 ${date(times.at(-1))}` : '';
    $('version-label').textContent = `QUOTA GUARD / ${state.version}`;
    markup($('accounts'), accounts.length ? accounts.map((s,i) => {
      const a = s.account, r = ruleFor(a), reset = s.reset, custom = !!Object.keys(r?.windows || {}).length;
      const name = esc(a.name || a.id), enabled = !!r?.enabled;
      const status = a.disabled ? '账号已禁用' : s.would_block ? (policy.mode === 'enforce' ? '额度保护中' : '观察：将避让') : enabled ? '额度充足' : '未启用保护';
      const buckets = s.buckets || [];
      return `<article class="panel account" data-account="${i}" data-key="${esc(a.id || a.name)}"><div class="account-head"><div class="account-title"><div class="avatar"><svg viewBox="0 0 24 24" aria-hidden="true">${a.provider === 'codex' ? '<path d="m8 6-6 6 6 6m8-12 6 6-6 6M14 4l-4 16"/>' : '<path d="m12 3 9 16H3L12 3Zm0 6v4m0 3h.01"/>'}</svg></div><div><h3>${name}</h3><small>${esc(a.provider?.toUpperCase())} · ${a.disabled ? 'DISABLED' : 'CONNECTED'}</small></div></div><span class="badge ${s.would_block ? 'warn' : enabled ? 'good' : ''}">${status}</span></div><div class="account-body"><div class="quota-side"><div class="side-title">额度窗口<label class="toggle"><input type="checkbox" data-edit="guard" ${enabled ? 'checked' : ''}><span class="toggle-track" aria-hidden="true"></span><span>启用额度保护</span></label></div>${buckets.length ? buckets.map((b,bi) => {
        const percent = Math.max(0,Math.min(100,Number(b.remaining_percent)||0));
        return `<div class="bucket" data-key="${esc(b.id)}"><div class="bucket-line"><span>${esc(b.label)}${b.auxiliary ? ' · 附加' : ''}</span><strong>${percent.toFixed(1)}%</strong></div><div class="bar ${percent <= (r?.windows?.[b.id] ?? r?.min_remaining_percent ?? 20) + policy.safety_percent ? 'low' : percent <= 50 ? 'medium' : ''}"><span style="width:${percent}%"></span></div><div class="bucket-meta"><span>剩余可用额度</span><span>${validDate(b.reset_at) ? `重置 ${esc(date(b.reset_at))}` : ''}</span></div>${custom ? `<label class="window-edit"><input type="checkbox" data-edit="window" data-bucket="${bi}" ${Object.hasOwn(r.windows,b.id) ? 'checked' : ''}>保护此窗口<input aria-label="${esc(b.label)}最低保留百分比" type="number" min="0" max="100" step="0.1" data-edit="window-min" data-bucket="${bi}" value="${Object.hasOwn(r.windows,b.id) ? r.windows[b.id] : r.min_remaining_percent}" ${Object.hasOwn(r.windows,b.id) ? '' : 'disabled'}>%</label>` : ''}</div>`;
      }).join('') : '<div class="empty-inline">尚无成功的额度查询</div>'}<div class="quota-options"><label>通用保留比例<input aria-label="${name}通用保留比例" type="number" min="0" max="100" step="0.1" data-edit="minimum" value="${r?.min_remaining_percent ?? 20}"> %</label><label><input type="checkbox" data-edit="custom" ${custom ? 'checked' : ''}> 单独选择额度窗口</label></div><div class="account-note ${s.error ? 'error' : ''}">${esc(s.error || s.reason)}</div></div><div class="reset-side"><div class="side-title">${reset ? 'Codex 重置卡' : '重置卡自动使用'}${reset ? `<label class="toggle"><input type="checkbox" data-edit="auto" ${accountAuto(a) ? 'checked' : ''} ${a.disabled ? 'disabled' : ''}><span class="toggle-track" aria-hidden="true"></span><span>此账号启用</span></label>` : ''}</div>${reset ? `<p class="account-note ${reset.error ? 'error' : ''}">${esc(reset.error || reset.message || '正在查询重置卡')}</p>${!draft.auto_reset.enabled ? '<p class="hint">总开关已关闭，此账号设置会保留。</p>' : ''}${reset.credits?.filter(c => Date.parse(c.expires_at) > Date.now()).map((c,ci) => {
        const at = new Date(Date.parse(c.expires_at) - policy.auto_reset.before_hours * 3600000).toISOString();
        return `<div class="credit"><div class="credit-row"><strong>可用重置卡 ${ci+1}</strong><span class="badge">${Date.parse(at) <= Date.now() ? '已进入窗口' : '待使用'}</span></div><small>到期 ${esc(date(c.expires_at))}</small><small>计划 ${esc(date(at))} · ${reset.enabled ? '后台自动' : '未启用'}</small><div class="meta">距使用窗口 <span data-countdown="${esc(at)}">${remaining(at)}</span></div></div>`;
      }).join('') || '<div class="empty-inline">暂无未到期的可用重置卡</div>'}<p class="account-note">检查时间 ${esc(date(reset.checked_at))}</p>` : '<div class="empty-inline">自动重置仅适用于 Codex 账号。</div>'}</div></div></article>`;
    }).join('') : '<div class="panel empty">尚未发现账号，请在 CPAMC 中添加授权。</div>');
    const events = state.reset_events || [];
    $('event-count').textContent = events.length;
    const labels = {pending:'等待核实',confirmed:'重置已确认',success:'核实完成',deferred:'延后重试',expired:'目标卡已到期',mismatch:'账号已暂停'};
    markup($('events'), events.length ? [...events].reverse().map(e => `<div class="event"><time>${esc(date(e.at))}</time><div class="event-account">${esc(e.account)}</div><div><span class="badge ${['success','confirmed'].includes(e.state) ? 'good' : ['mismatch','expired'].includes(e.state) ? 'warn' : ''}">${esc(labels[e.state] || e.state)}</span><p>${esc(e.message)}</p></div></div>`).join('') : '<div class="empty-inline">暂无执行记录。进入提前使用窗口后，后台会在这里记录结果。</div>');
    if (state.reset_error || state.refresh_error) message(state.reset_error || state.refresh_error, true);
  }
  async function load(discardDraft = false) {
    if (loading) return; loading = true;
    const initial = !connected; busy('reload', true);
    try {
      const result = await request('status');
      if (!initial && !discardDraft && (dirty || document.activeElement.matches('input,select'))) return;
      state = result; state.policy.auto_reset ||= {enabled:false,before_hours:6,accounts:{}};
      state.policy.auto_reset.accounts ||= {};
      draft = copy(state.policy); dirty = false; connected = true;
      $('session').hidden = true; $('content').hidden = false; $('toolbar').hidden = false;
      $('dirty').textContent = ''; $('save').disabled = true; message(); render(); if (initial) reveal();
    } catch (error) { if (!connected) disconnect(error.message); else message(error.message, true); }
    finally { loading = false; busy('reload', false); }
  }
  $('reconnect').onclick = () => { request('appearance').then(appearance).catch(() => {}); load(); };
  $('reload').onclick = () => load(true);
  $('probe').onclick = async () => { busy('probe',true); try { await request('refresh'); message('后台刷新已排队。'); if (!dirty) setTimeout(() => { if (!dirty && !document.activeElement.matches('input,select')) load(); },1500); } catch(error) { message(error.message,true); } finally { busy('probe',false); } };
  $('save').onclick = async () => {
    const invalid = [...document.querySelectorAll('input[type=number]')].find(input => !input.disabled && !input.checkValidity());
    if (invalid) { document.querySelector(`[data-tab="${invalid.closest('.view')?.id || 'accounts-view'}"]`)?.click(); invalid.reportValidity(); return; }
    busy('save', true);
    try { const result = await request('policy',draft); state = result; draft = copy(result.policy); dirty = false; $('dirty').textContent = ''; message('设置已保存，服务器后台已按新策略运行。'); render(); }
    catch(error) { message(error.message,true); }
    finally { busy('save', false); $('save').disabled = !dirty; }
  };
  document.querySelectorAll('[data-tab]').forEach(button => { button.onclick = () => {
    if (button.getAttribute('aria-selected') === 'true') return;
    document.querySelectorAll('[data-tab]').forEach(b => { b.classList.toggle('active',b===button); b.setAttribute('aria-selected',String(b===button)); b.tabIndex = b===button ? 0 : -1; });
    document.querySelectorAll('.view').forEach(view => { view.hidden = view.id !== button.dataset.tab; view.classList.remove('is-entering'); });
    if (!reducedMotion()) $(button.dataset.tab).classList.add('is-entering');
  }; button.onkeydown = event => {
    const tabs = [...document.querySelectorAll('[data-tab]')], index = tabs.indexOf(button);
    const target = event.key === 'Home' ? tabs[0] : event.key === 'End' ? tabs.at(-1) : event.key === 'ArrowRight' ? tabs[(index+1)%tabs.length] : event.key === 'ArrowLeft' ? tabs[(index+tabs.length-1)%tabs.length] : null;
    if (target) { event.preventDefault(); target.focus(); target.click(); }
  }; });
  document.querySelectorAll('[data-field]').forEach(input => { input.onchange = () => { draft[input.dataset.field] = input.type === 'number' ? Number(input.value) : input.value; setDirty(); }; });
  $('auto-enabled').onchange = event => { draft.auto_reset.enabled = event.target.checked; setDirty(); render(); };
  $('auto-hours').onchange = event => { draft.auto_reset.before_hours = Number(event.target.value); setDirty(); };
  $('accounts').addEventListener('change', event => {
    const input = event.target, kind = input.dataset.edit, node = input.closest('[data-account]'); if (!kind || !node) return;
    const s = state.accounts[Number(node.dataset.account)], a = s.account;
    if (kind === 'auto') { Object.defineProperty(draft.auto_reset.accounts,autoKey(a),{value:input.checked,writable:true,enumerable:true,configurable:true}); setDirty(); return; }
    const r = ruleFor(a,true);
    if (kind === 'guard') r.enabled = input.checked;
    if (kind === 'minimum') r.min_remaining_percent = Number(input.value);
    if (kind === 'custom') { if (input.checked) { r.windows = Object.fromEntries((s.buckets || []).filter(b => !b.auxiliary).map(b => [b.id,r.min_remaining_percent])); if (!Object.keys(r.windows).length) { message('查询到额度窗口后才能单独选择。',true); input.checked = false; return; } } else delete r.windows; }
    if (kind === 'window' || kind === 'window-min') {
      const bucket = s.buckets[Number(input.dataset.bucket)];
      if (kind === 'window') { if (input.checked) Object.defineProperty(r.windows,bucket.id,{value:r.min_remaining_percent,writable:true,enumerable:true,configurable:true}); else { if (Object.keys(r.windows).length === 1) { input.checked = true; message('至少保留一个窗口，或关闭单独选择。',true); return; } delete r.windows[bucket.id]; } }
      else r.windows[bucket.id] = Number(input.value);
    }
    setDirty(); if (['custom','window'].includes(kind)) render();
  });
  window.addEventListener('beforeunload', event => { if (dirty) { event.preventDefault(); event.returnValue = ''; } });
  setInterval(() => { if (connected && !dirty && !document.hidden && !document.activeElement.matches('input,select')) load(); },15000);
  setInterval(countdowns,30000);
  request('appearance').then(appearance).catch(() => {}); load();
})();

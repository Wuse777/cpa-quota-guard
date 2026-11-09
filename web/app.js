(() => {
  'use strict';
  const M = globalThis.QuotaUI, $ = id => document.getElementById(id);
  const channel = 'quota-guard:cpamc-session:v1', pending = new Map();
  const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const date = value => M.validDate(value) ? new Date(value).toLocaleString('zh-CN', {hour12:false}) : '尚无记录';
  const percent = value => Math.max(0,Math.min(100,Number(value)||0));
  const number = value => Number(value.toFixed(1));
  const healthLabels = {healthy:'查询正常',stale:'额度已过期',error:'查询失败',unknown:'额度未知',disabled:'账号停用',unsupported:'尚未适配'};
  const schedulerLabels = {disabled:'未启用',waiting:'等待使用窗口',ready:'已进入窗口',retry_wait:'等待重试',verifying:'正在核实',blocked:'排程已暂停',no_credits:'暂无卡片',error:'查询异常'};
  const eventLabels = {pending:'等待核实',confirmed:'重置已确认',success:'核实完成',deferred:'延后重试',expired:'目标卡已到期',mismatch:'账号已暂停'};
  let state, basePolicy, draft, dirty=false, connected=false, loading=false, saving=false, discarding=false, serial=0, receivedAt;
  const reducedMotion = () => window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  const remaining = value => {
    if (!M.validDate(value)) return '—';
    const seconds = Math.ceil((Date.parse(value)-Date.now())/1000);
    if (seconds <= 0) return '已进入使用窗口';
    const hours=Math.floor(seconds/3600), minutes=Math.floor(seconds%3600/60);
    return hours >= 24 ? `${Math.floor(hours/24)} 天 ${hours%24} 小时` : `${hours} 小时 ${minutes} 分`;
  };
  // Keyed, incremental DOM updates retain focus, open timelines and unsaved input.
  function markup(container, html) {
    const template=document.createElement('template'); template.innerHTML=html;
    function patch(parent,desired) {
      [...desired.childNodes].forEach((next,i) => {
        let current=parent.childNodes[i], key=next.nodeType===1 && next.getAttribute('data-key');
        if (key && current?.getAttribute?.('data-key')!==key) {
          const existing=[...parent.childNodes].slice(i+1).find(n => n.nodeType===1 && n.getAttribute('data-key')===key);
          if (existing) { parent.insertBefore(existing,current); current=existing; }
        }
        if (!current) { parent.append(next.cloneNode(true)); return; }
        if (current.nodeType!==next.nodeType || current.nodeName!==next.nodeName ||
            current.nodeType===1 && current.getAttribute('data-key')!==next.getAttribute('data-key')) {
          current.replaceWith(next.cloneNode(true)); return;
        }
        if (next.nodeType!==1) { if(current.nodeValue!==next.nodeValue)current.nodeValue=next.nodeValue; return; }
        const preserveOpen=current instanceof HTMLDetailsElement;
        for(const attr of [...current.attributes]) if(!next.hasAttribute(attr.name) && !(preserveOpen && attr.name==='open'))current.removeAttribute(attr.name);
        for(const attr of next.attributes) if(!(preserveOpen && attr.name==='open') && current.getAttribute(attr.name)!==attr.value)current.setAttribute(attr.name,attr.value);
        if(current instanceof HTMLInputElement && current!==document.activeElement) {
          if(current.value!==next.value)current.value=next.value;
          current.checked=next.checked;
        }
        patch(current,next);
      });
      while(parent.childNodes.length>desired.childNodes.length)parent.lastChild.remove();
    }
    patch(container,template.content);
  }
  function message(text='',error=false) { $('message').textContent=text; $('message').classList.toggle('error',error); }
  function busy(id,value) { $(id).disabled=value; $(id).setAttribute('aria-busy',String(value)); }
  function disconnect(text) { connected=false; $('session').hidden=false; $('session').querySelector('h2').textContent='连接管理面板后继续'; $('session').querySelector('p').textContent=text; $('content').hidden=true; $('toolbar').hidden=true; }
  function request(operation,body) {
    return new Promise((resolve,reject) => {
      if(window.parent===window) {reject(new Error('请在 CPAMC 的「账号额度保护」页面使用插件。'));return;}
      const id=`qg-${Date.now()}-${++serial}`;
      const timer=setTimeout(() => {pending.delete(id);reject(new Error('管理面板连接超时，请刷新 CPAMC 页面后重试。'));},25000);
      pending.set(id,{resolve,reject,timer});
      window.parent.postMessage({channel,type:'request',id,operation,body},location.origin);
    });
  }
  function appearance(data) {
    if(!data?.colors)return;
    const vars=['--bg-primary','--bg-secondary','--bg-tertiary','--bg-hover','--text-primary','--text-secondary','--text-tertiary','--border-color','--primary-color','--primary-hover','--primary-contrast','--success-color','--warning-color','--error-color','--shadow','--shadow-lg','--border-hover','--primary-active','--viz-success','--viz-failure','--quota-medium-color','--success-badge-bg','--success-badge-text','--success-badge-border','--failure-badge-bg','--failure-badge-text','--failure-badge-border','--ease-out-strong','--dur-press','--dur-hover'];
    for(const key of vars)if(typeof data.colors[key]==='string' && data.colors[key])document.documentElement.style.setProperty(key,data.colors[key]);
    for(const [field,variable]of Object.entries({fontFamily:'--cpamc-font-family',fontSize:'--cpamc-font-size',lineHeight:'--cpamc-line-height'}))if(data.typography?.[field])document.documentElement.style.setProperty(variable,data.typography[field]);
    for(const [field,variable]of Object.entries({inline:'--cpamc-page-padding-inline',top:'--cpamc-page-padding-top',bottom:'--cpamc-page-padding-bottom'}))if(data.layout?.[field])document.documentElement.style.setProperty(variable,data.layout[field]);
    document.documentElement.style.colorScheme=data.theme==='dark'?'dark':'light';document.documentElement.dataset.theme=data.theme;
  }
  window.addEventListener('message',event => {
    if(event.source!==window.parent || event.origin!==location.origin || event.data?.channel!==channel)return;
    const reply=event.data;if(reply.type==='appearance'){appearance(reply.data);return;}
    if(reply.type!=='response' || !pending.has(reply.id))return;
    const task=pending.get(reply.id);pending.delete(reply.id);clearTimeout(task.timer);
    if(reply.status>=200 && reply.status<300)task.resolve(reply.data);
    else {const error=new Error(reply.data?.error || '插件请求失败');error.status=reply.status;if(reply.status===401)disconnect(error.message);task.reject(error);}
  });
  function ruleFor(account,create=false) {
    let rule=M.ruleFor(draft,account);
    if(!rule && create){rule={account:account.name||account.id,enabled:false,min_remaining_percent:20};draft.rules.push(rule);}
    return rule;
  }
  const autoKey=account => M.keys(account).find(key => Object.hasOwn(draft.auto_reset.accounts,key)) || account.name || account.id;
  function syncInput(input,value) {if(input===document.activeElement)return;if(input.type==='checkbox')input.checked=!!value;else if(input.value!==String(value))input.value=value;}
  function updateDraftStatus() {
    if(!draft)return;
    const delta=M.changes(basePolicy,draft,state.accounts||[]);dirty=delta.count>0;
    $('savebar').hidden=!dirty||!connected;$('dirty').textContent=`${delta.count} 项未保存修改`;$('impact').textContent=`预计影响 ${delta.impacted} 个启用账号`;
    $('conflict').hidden=!(dirty && draft.revision!==state.policy.revision);
    document.body.classList.toggle('has-draft',dirty);
    for(const id of ['save','save-bottom']){$(id).disabled=!dirty || saving || discarding;$(id).setAttribute('aria-busy',String(saving));$(id).textContent=saving?'正在保存…':'保存设置';}
    $('discard').disabled=saving||discarding;$('reload').disabled=loading||saving||discarding;
    $('policy-settings').disabled=saving||discarding;
    document.querySelectorAll('.account-fields').forEach(fieldset => {fieldset.disabled=saving||discarding;});
  }
  function renderOverview() {
    const p=state.policy,s=M.summary(state),accounts=state.accounts||[];
    $('mode-label').textContent=p.mode==='enforce'?'执行模式':'观察模式';
    $('rule-count').textContent=`生效规则 ${s.effectiveRules} / ${s.rules}`;
    $('account-count').textContent=s.total;$('account-tab-count').textContent=s.total;$('account-breakdown').textContent=`启用 ${s.enabled} · 停用 ${s.disabled}`;
    $('held-label').textContent=p.mode==='enforce'?'正在保护':'预计避让';$('held-count').textContent=s.held;
    const unhealthy=accounts.filter(a => !a.account.disabled && M.health(a,p)!=='healthy').length;
    $('health-summary').textContent=unhealthy?`${unhealthy} 个启用账号额度需关注`:'启用账号查询正常';
    $('credit-count').textContent=s.cards;$('auto-label').textContent=`可自动处理 ${s.automaticCards} 张`;
    const next=s.next,paused=!!state.reset_error || s.blocked.length>0;
    document.querySelector('.upcoming').classList.toggle('has-issue',paused);
    $('next-title').textContent=next?`${next.account.name} · 下一次自动处理`:p.auto_reset.enabled?'暂无可执行的重置计划':'自动使用已关闭';
    $('next-description').textContent=next?(next.reset.message||schedulerLabels[M.scheduler(next)]):state.reset_error||s.blocked[0]?.reset?.blocked_reason||'启用账号并开启自动使用后，服务器会在此显示排程。';
    $('next-state').textContent=next?schedulerLabels[M.scheduler(next)]||'已安排':paused?'需要处理':p.auto_reset.enabled?'持续检查':'未启用';
    $('next-state').className='badge'+(paused?' warn':next?' good':'');
    $('next-countdown').textContent=next?remaining(next.reset.next_at):'—';
    if(next)$('next-countdown').dataset.countdown=next.reset.next_at;else delete $('next-countdown').dataset.countdown;
    $('next-date').textContent=next?`计划 ${date(next.reset.next_at)}`:'查看账号卡片了解当前状态';
    const history=accounts.filter(a => !a.account.disabled && a.reset?.last_result?.error).sort((a,b) => Date.parse(b.reset.last_result.at)-Date.parse(a.reset.last_result.at));
    $('history-warning').hidden=!history.length;
    if(history.length){const last=history[0];$('history-warning').textContent=`上次兑换异常 · ${date(last.reset.last_result.at)} · ${last.account.name}。历史记录保留，当前排程单独显示。`;}
    const warnings=[state.refresh_error,state.reset_error,p.mode==='enforce' && !s.effectiveRules?'执行模式尚无生效账号规则，请在账号卡片启用额度保护。':''].filter(Boolean);
    $('runtime-warning').hidden=!warnings.length;$('runtime-warning').textContent=warnings.join(' ');
    $('updated-at').textContent=`状态更新 ${date(receivedAt)}`;
    $('version-label').textContent=`QUOTA GUARD / ${state.version}`;
  }
  function quotaMarkup(s,r) {
    const a=s.account,custom=!!Object.keys(r?.windows||{}).length;
    const buckets=(s.buckets||[]).map((b,bi) => {
      const t=M.threshold(draft,r,b),v=percent(b.remaining_percent), low=t.protected && v<=t.stop;
      return `<div class="bucket" data-key="${esc(b.id)}"><div class="bucket-line"><span>${esc(b.label)}${b.auxiliary?' · 附加':''}</span><strong>${v.toFixed(1)}%</strong></div><div class="bar ${!t.protected?'unprotected':low?'low':v<=50?'medium':''}" role="meter" aria-label="${esc(b.label)}剩余额度" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${v}"><span style="width:${v}%"></span>${t.protected?`<i class="stop-marker" style="left:${t.stop}%" title="停止线 ${t.stop}%"></i><i class="resume-marker" style="left:${t.resume}%" title="恢复线 ${t.resume}%"></i>`:''}</div><div class="bucket-meta"><span>${t.protected?`停止 ≤${number(t.stop)}% · 恢复 ≥${number(t.resume)}%`:'仅展示 · 不参与保护'}</span><span>${M.validDate(b.reset_at)?`重置 ${esc(date(b.reset_at))}`:''}</span></div>${t.permanent?'<p class="threshold-warning" role="note">停止线为 100%：保存执行策略后，此窗口会持续阻断账号。</p>':''}${custom?`<div class="window-edit"><label><input type="checkbox" data-edit="window" data-bucket="${bi}" ${Object.hasOwn(r.windows,b.id)?'checked':''}>保护此窗口</label><label class="sr-only" for="window-${esc(a.id)}-${bi}">${esc(b.label)}最低保留百分比</label><input id="window-${esc(a.id)}-${bi}" aria-label="${esc(b.label)}最低保留百分比" type="number" min="0" max="100" step="0.1" data-edit="window-min" data-bucket="${bi}" value="${Object.hasOwn(r.windows,b.id)?r.windows[b.id]:r.min_remaining_percent}" ${Object.hasOwn(r.windows,b.id)?'':'disabled'}><span>%</span></div>`:''}</div>`;
    }).join('') || '<div class="empty-inline">尚无成功的额度查询，不能据此判断额度充足。</div>';
    return `<div class="side-title">额度窗口<label class="toggle"><input type="checkbox" data-edit="guard" ${r?.enabled?'checked':''}><span class="toggle-track" aria-hidden="true"></span><span>额度保护</span></label></div>${buckets}<div class="quota-options"><label>通用保留比例 <input aria-label="${esc(a.name)}通用保留比例" type="number" min="0" max="100" step="0.1" required data-edit="minimum" value="${r?.min_remaining_percent??20}"> %</label><label><input type="checkbox" data-edit="custom" ${custom?'checked':''}>单独选择窗口</label></div><p class="account-note">${esc(s.reason||'未启用额度保护')}</p>${s.error?`<p class="account-note error">查询错误：${esc(s.error)}</p>`:''}<div class="query-times"><span>上次成功 ${esc(date(s.checked_at))}</span><span>最近尝试 ${esc(date(s.attempt_at))}</span></div>`;
  }
  function resetMarkup(s) {
    const a=s.account,r=s.reset;if(!r)return '<div class="side-title">重置卡</div><p class="empty-inline">自动重置仅适用于 Codex 账号。</p>';
    const schedule=M.scheduler(s),history=r.last_result;
    const cards=M.liveCredits(s).map((c,i) => {const at=new Date(Date.parse(c.expires_at)-state.policy.auto_reset.before_hours*3600000).toISOString();return `<div class="credit" data-key="${esc(c.id||i)}"><div class="credit-row"><strong>重置卡 ${i+1}</strong><span class="badge">${a.disabled?'停用账号快照':M.actionable(s)?'已纳入排程':'未自动处理'}</span></div><small>到期 ${esc(date(c.expires_at))}</small><small>使用窗口 ${esc(date(at))}</small></div>`;}).join('');
    return `<div class="side-title">Codex 重置卡<label class="toggle"><input type="checkbox" data-edit="auto" ${M.autoEnabled(draft,a)?'checked':''} ${a.disabled?'disabled':''}><span class="toggle-track" aria-hidden="true"></span><span>此账号启用</span></label></div><div class="reset-current"><span class="badge ${['blocked','error'].includes(schedule)?'warn':M.actionable(s)?'good':''}">${schedulerLabels[schedule]||esc(schedule)}</span><p>${esc(r.blocked_reason||r.query_error||r.message||'等待后台检查')}</p>${M.validDate(r.next_at)&&M.actionable(s)?`<small>下一次 ${esc(date(r.next_at))}</small>`:''}</div>${history?.error?`<details class="history-detail"><summary>上次兑换异常 · ${esc(date(history.at))}</summary><p class="error">${esc(history.error)}</p><p class="meta">${esc(resultText(history))}</p><p class="hint">历史错误保留；是否暂停以当前排程状态为准。</p></details>`:''}${!draft.auto_reset.enabled?'<p class="hint">自动使用总开关已关闭；账号单独设置保留。</p>':''}${cards||'<div class="empty-inline">暂无未过期的重置卡快照</div>'}<p class="query-times">卡片上次成功查询 ${esc(date(r.checked_at))}</p>`;
  }
  function renderAccounts() {
    const accounts=state.accounts||[],p=state.policy,query=$('account-search').value.trim(),filter=$('account-filter').value;
    const rank=s => s.account.disabled?3:['error','stale','unknown'].includes(M.health(s,p))||['blocked','error'].includes(M.scheduler(s))?0:1;
    let visible=accounts.filter(s => M.accountMatches(s,query,filter,p)).sort((a,b) => rank(a)-rank(b)||a.account.name.localeCompare(b.account.name));
    // Keep the active editor in place if a poll changes its health/sort rank.
    if($('accounts').contains(document.activeElement)){const order=[...$('accounts').children].map(n => n.dataset.key);visible.sort((a,b) => order.indexOf(a.account.id)-order.indexOf(b.account.id));}
    $('account-results').textContent=`显示 ${visible.length} / ${accounts.length} 个`;
    markup($('accounts'),visible.map(s => {
      const a=s.account,r=ruleFor(a),h=M.health(s,p),liveRule=M.ruleFor(p,a),status=a.disabled?'账号停用':s.would_block?(p.mode==='enforce'?'额度保护中':'观察：预计避让'):!liveRule?.enabled?'未启用保护':h==='healthy'?'额度充足':'额度待核实';
      const changed=JSON.stringify(liveRule)!==JSON.stringify(r)||M.autoEnabled(p,a)!==M.autoEnabled(draft,a);
      return `<details class="panel account ${a.disabled?'is-disabled':''}" data-key="${esc(a.id)}" data-account-id="${esc(a.id)}" ${a.disabled?'':'open'}><summary class="account-head"><div class="account-title"><span class="avatar" aria-hidden="true">${a.provider==='codex'?'⌘':'△'}</span><div><h3>${esc(a.name||a.id)}</h3><small>${esc(a.provider?.toUpperCase())} · ${a.disabled?'已停用':'已启用'}</small></div></div><div class="account-badges"><span class="badge ${h==='healthy'?'good':['error','stale','unknown'].includes(h)?'warn':''}">${healthLabels[h]||'额度未知'}</span><span class="badge ${s.would_block?'warn':''}">${status}</span>${changed?'<span class="badge draft-badge">有草稿</span>':''}<span class="chevron" aria-hidden="true">⌄</span></div></summary><fieldset class="account-fields" ${saving||discarding?'disabled':''}><legend class="sr-only">${esc(a.name)}策略</legend><div class="account-body"><div class="quota-side">${quotaMarkup(s,r)}</div><div class="reset-side">${resetMarkup(s)}</div></div></fieldset></details>`;
    }).join('')||'<div class="panel empty">没有匹配的账号。</div>');
  }
  const resultText=result => [result.management_status?`CPA HTTP ${result.management_status}`:'',result.http_status?`上游 HTTP ${result.http_status}`:'',result.code||''].filter(Boolean).join(' · ');
  function filteredGroups() {const query=$('event-search').value.trim().toLowerCase(),filter=$('event-filter').value;return M.groups(state.reset_events).filter(g => (!query||g.account.toLowerCase().includes(query)) && (filter==='all'||filter==='success'&&['success','confirmed'].includes(g.latest.state)||filter==='expired'&&g.latest.state==='expired'||filter==='attention'&&!['success','confirmed'].includes(g.latest.state)));}
  function renderEvents() {
    const groups=filteredGroups();$('event-count').textContent=M.groups(state.reset_events).length;
    markup($('events'),groups.map(g => `<details class="panel operation" data-key="${esc(g.key)}"><summary><div><strong>${esc(g.account)}</strong><span class="meta">${esc(date(g.latest.at))} · ${g.events.length} 条事件</span></div><span class="badge ${['success','confirmed'].includes(g.latest.state)?'good':['mismatch','expired'].includes(g.latest.state)||g.latest.result?.error?'warn':''}">${eventLabels[g.latest.state]||esc(g.latest.state)}</span></summary><p>${esc(g.latest.message)}</p><ol class="timeline">${g.events.map(e => `<li><time>${esc(date(e.at))}</time><strong>${eventLabels[e.state]||esc(e.state)}</strong><p>${esc(e.message)}</p>${e.result?`<p class="meta">${esc(resultText(e.result))}</p>${e.result.error?`<p class="error">${esc(e.result.error)}</p>`:''}`:''}</li>`).join('')}</ol></details>`).join('')||'<div class="panel empty">暂无匹配的执行记录。</div>');
  }
  function renderSettings() {
    document.querySelectorAll('[data-field]').forEach(input => syncInput(input,draft[input.dataset.field]));
    syncInput($('auto-enabled'),draft.auto_reset.enabled);syncInput($('auto-hours'),draft.auto_reset.before_hours);syncInput($('auto-include-new'),draft.auto_reset.include_new_accounts);
    const sample=M.threshold(draft,{enabled:true,min_remaining_percent:20},{id:'example'});
    $('threshold-example').textContent=`以保留 20% 为例：剩余 ≤${number(sample.stop)}% 停止分配，恢复至 ≥${number(sample.resume)}% 后重新使用。${sample.permanent?'停止线达到100%，账号会持续被阻断。':''}`;
    const build=state.build||{version:state.version},health=state.health||{};
    markup($('build-info'),[['插件版本',build.version||state.version],['构建提交',build.commit||'未提供'],['构建时间',M.validDate(build.built_at)?date(build.built_at):build.built_at||'未提供'],['Go / 桥接版本',`${build.go_version||'未提供'} / ${build.bridge_version||'未提供'}`],['CPA 兼容范围',build.cpa_compatibility||'以安装文档为准'],['CPAMC 兼容范围',build.cpamc_compatibility||'以安装文档为准'],['额度最近成功',date(health.last_quota_success_at)],['卡片最近成功',date(health.last_reset_success_at)]].map(([k,v]) => `<div><dt>${esc(k)}</dt><dd>${esc(v)}</dd></div>`).join(''));
    const n=state.notifications||{};$('notification-status').textContent=n.enabled?`已启用 · ${n.health==='healthy'?'投递正常':n.health||'等待事件'}${n.last_sent_at?` · 上次发送 ${date(n.last_sent_at)}`:''}${n.last_error?` · ${n.last_error}`:''}`:'尚未启用；异常仍会保存在执行记录中。';
  }
  function render() {if(!state||!draft)return;renderOverview();renderAccounts();renderEvents();renderSettings();updateDraftStatus();}
  async function load(discard=false) {
    if(loading)return false;loading=true;discarding=discard;updateDraftStatus();busy('reload',true);
    try {
      const result=await request('status');result.policy=M.policy(result.policy);
      if(state && result.policy.revision<state.policy.revision)return false;
      state=result;receivedAt=new Date().toISOString();
      if(!draft||discard||(!dirty && !saving && !document.activeElement.matches('[data-edit],[data-field],#auto-enabled,#auto-hours,#auto-include-new'))){draft=M.copy(state.policy);basePolicy=M.copy(state.policy);}
      connected=true;$('session').hidden=true;$('content').hidden=false;$('toolbar').hidden=false;
      render();return true;
    }catch(error){if(!connected)disconnect(error.message);else message(error.message,true);return false;}
    finally{loading=false;discarding=false;updateDraftStatus();}
  }
  async function save() {
    if(saving||!dirty)return;
    const invalid=[...document.querySelectorAll('input[type=number]')].find(input => !input.disabled&&!input.checkValidity());
    if(invalid){document.querySelector(`[data-tab="${invalid.closest('.view')?.id||'accounts-view'}"]`)?.click();invalid.closest('details.account,details.advanced')?.setAttribute('open','');invalid.reportValidity();return;}
    saving=true;updateDraftStatus();
    const payload=M.copy(draft);
    try {const result=await request('policy',payload);result.policy=M.policy(result.policy);state=result;draft=M.copy(result.policy);basePolicy=M.copy(result.policy);receivedAt=new Date().toISOString();message('设置已保存，服务器后台已按新策略运行。');}
    catch(error){message(error.status===409?'服务器策略已被其他页面修改。草稿已保留；请记录修改后重新载入并核对。':error.message,true);if(error.status===409)await load();}
    finally{saving=false;render();}
  }
  function edit() {if(saving||discarding)return;updateDraftStatus();renderAccounts();renderSettings();updateDraftStatus();}
  $('save').onclick=save;$('save-bottom').onclick=save;
  $('reload').onclick=() => {if(dirty)$('discard-dialog').showModal();else load(true);};
  $('discard').onclick=() => $('discard-dialog').showModal();
  $('cancel-discard').onclick=() => $('discard-dialog').close();
  $('confirm-discard').onclick=async() => {$('discard-dialog').close();if(await load(true))message('已放弃修改并重新载入服务器策略。');};
  $('reconnect').onclick=() => {request('appearance').then(appearance).catch(()=>{});load();};
  $('probe').onclick=async() => {busy('probe',true);try{await request('refresh');message('后台刷新已排队，运行状态会继续更新。');await load();setTimeout(()=>load(),1500);}catch(error){message(error.message,true);}finally{busy('probe',false);}};
  for(const id of ['account-search','account-filter'])$(id).addEventListener(id.endsWith('search')?'input':'change',renderAccounts);
  for(const id of ['event-search','event-filter'])$(id).addEventListener(id.endsWith('search')?'input':'change',renderEvents);
  document.querySelectorAll('[data-field]').forEach(input => input.addEventListener(input.type==='number'?'input':'change',() => {if(saving||discarding)return;draft[input.dataset.field]=input.type==='number'?Number(input.value):input.value;edit();}));
  for(const [id,key]of [['auto-enabled','enabled'],['auto-hours','before_hours'],['auto-include-new','include_new_accounts']])$(id).addEventListener(id==='auto-hours'?'input':'change',event => {if(saving||discarding)return;draft.auto_reset[key]=event.target.type==='checkbox'?event.target.checked:Number(event.target.value);edit();});
  function accountEdit(event) {
    if(saving||discarding)return;
    const input=event.target,kind=input.dataset.edit,node=input.closest('[data-account-id]');if(!kind||!node)return;
    if(event.type==='input'&&input.type!=='number'||event.type==='change'&&input.type==='number')return;
    const s=state.accounts.find(s => s.account.id===node.dataset.accountId);if(!s)return;const a=s.account;
    if(kind==='auto'){Object.defineProperty(draft.auto_reset.accounts,autoKey(a),{value:input.checked,writable:true,enumerable:true,configurable:true});edit();return;}
    const r=ruleFor(a,true);
    if(kind==='guard')r.enabled=input.checked;
    if(kind==='minimum')r.min_remaining_percent=Number(input.value);
    if(kind==='custom'){
      if(input.checked){r.windows=Object.fromEntries((s.buckets||[]).filter(b=>!b.auxiliary).map(b=>[b.id,r.min_remaining_percent]));if(!Object.keys(r.windows).length){delete r.windows;input.checked=false;message('查询到额度窗口后才能单独选择。',true);return;}}
      else delete r.windows;
    }
    if(kind==='window'||kind==='window-min'){const bucket=s.buckets[Number(input.dataset.bucket)];if(kind==='window'){if(input.checked)Object.defineProperty(r.windows,bucket.id,{value:r.min_remaining_percent,writable:true,enumerable:true,configurable:true});else{if(Object.keys(r.windows).length===1){input.checked=true;message('至少保留一个窗口，或关闭单独选择。',true);return;}delete r.windows[bucket.id];}}else r.windows[bucket.id]=Number(input.value);}
    edit();
  }
  $('accounts').addEventListener('change',accountEdit);$('accounts').addEventListener('input',accountEdit);
  document.querySelectorAll('[data-tab]').forEach(button => {
    button.onclick=() => {if(button.getAttribute('aria-selected')==='true')return;document.querySelectorAll('[data-tab]').forEach(b=>{b.classList.toggle('active',b===button);b.setAttribute('aria-selected',String(b===button));b.tabIndex=b===button?0:-1;});document.querySelectorAll('.view').forEach(view=>{view.hidden=view.id!==button.dataset.tab;view.classList.remove('is-entering');});if(!reducedMotion())$(button.dataset.tab).classList.add('is-entering');};
    button.onkeydown=event => {const tabs=[...document.querySelectorAll('[data-tab]')],index=tabs.indexOf(button),target=event.key==='Home'?tabs[0]:event.key==='End'?tabs.at(-1):event.key==='ArrowRight'?tabs[(index+1)%tabs.length]:event.key==='ArrowLeft'?tabs[(index+tabs.length-1)%tabs.length]:null;if(target){event.preventDefault();target.focus();target.click();}};
  });
  document.addEventListener('animationend',event=>{if(event.animationName==='page-enter')event.target.classList.remove('is-entering');});
  $('export-events').onclick=() => {
    // Never export account identifiers, credit IDs or upstream response text.
    const aliases=new Map(),alias=name=>{if(!aliases.has(name))aliases.set(name,`账号 ${aliases.size+1}`);return aliases.get(name);};
    const records=filteredGroups().map(g=>({account:alias(g.account),events:g.events.map(e=>({at:e.at,state:e.state,management_status:e.result?.management_status,http_status:e.result?.http_status,code:e.result?.code}))}));
    const url=URL.createObjectURL(new Blob([JSON.stringify(records,null,2)],{type:'application/json'})),link=document.createElement('a');link.href=url;link.download='quota-guard-events-redacted.json';link.click();setTimeout(()=>URL.revokeObjectURL(url),1000);
  };
  window.addEventListener('beforeunload',event=>{if(dirty){event.preventDefault();event.returnValue='';}});
  document.addEventListener('visibilitychange',()=>{if(!document.hidden&&connected)load();});
  setInterval(()=>{if(connected&&!document.hidden)load();},15000);
  setInterval(()=>document.querySelectorAll('[data-countdown]').forEach(n=>{n.textContent=remaining(n.dataset.countdown);}),30000);
  if(window.parent!==window)document.documentElement.classList.add('embedded');
  request('appearance').then(appearance).catch(()=>{});load();
})();

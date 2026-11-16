/* Pure view calculations shared by the embedded UI and its regression tests. */
(() => {
  'use strict';
  const copy = value => JSON.parse(JSON.stringify(value));
  const validDate = value => !!value && !String(value).startsWith('0001-') && Number.isFinite(Date.parse(value));
  const keys = account => [account.name, account.id, account.auth_index].filter(Boolean);
  function policy(value) {
    const result = copy(value || {});
    result.rules ||= [];
    result.auto_reset ||= {enabled:false, before_hours:6, include_new_accounts:false, accounts:{}};
    result.auto_reset.accounts ||= {};
    result.auto_reset.include_new_accounts ??= false;
    result.auto_activate ||= {};
    for (const [key,value] of Object.entries({enabled:false,include_new_accounts:false,natural:true,reset:true,five_hour:true,weekly:true,model:''})) result.auto_activate[key] ??= value;
    result.auto_activate.accounts ||= {};
    result.auto_activate.account_models ||= {};
    return result;
  }
  const ruleFor = (p, account) => p.rules.find(rule => keys(account).includes(rule.account));
  function autoEnabled(p, account) {
    const key = keys(account).find(key => Object.hasOwn(p.auto_reset.accounts, key));
    return key ? p.auto_reset.accounts[key] : !!p.auto_reset.include_new_accounts;
  }
  function activationEnabled(p, account) {
    const activation = p.auto_activate || {};
    const key = keys(account).find(key => Object.hasOwn(activation.accounts || {},key));
    return key ? !!activation.accounts[key] : !!activation.include_new_accounts;
  }
  function activationModelFor(p, account) {
    const activation = p.auto_activate || {};
    const key = keys(account).find(key => Object.hasOwn(activation.account_models || {},key));
    return (key && activation.account_models[key]) || activation.model || '';
  }
  const activationAttention = snapshot => ['blocked','uncertain','failed'].includes(snapshot.activation?.state) || !!snapshot.activation?.last_result?.error;
  const activationWindow = value => ({five_hour:'五小时额度',weekly:'周额度','18000':'五小时额度','604800':'周额度'}[String(value)] || String(value));
  function health(snapshot, p, now = Date.now()) {
    if (snapshot.account.disabled) return 'disabled';
    if (snapshot.health) return snapshot.health;
    if (!['codex','antigravity'].includes(snapshot.account.provider)) return 'unsupported';
    if (snapshot.error) return 'error';
    if (!validDate(snapshot.checked_at)) return 'unknown';
    return now - Date.parse(snapshot.checked_at) > p.max_age_seconds * 1000 ? 'stale' : 'healthy';
  }
  const liveCredits = (snapshot, now = Date.now()) => (snapshot.reset?.credits || []).filter(c => Date.parse(c.expires_at) > now);
  const scheduler = snapshot => snapshot.account.disabled ? 'disabled' : snapshot.reset?.scheduler_state || (snapshot.reset?.enabled ? 'waiting' : 'disabled');
  const actionable = snapshot => !!snapshot.reset?.enabled && !snapshot.account.disabled && !['blocked','error','disabled'].includes(scheduler(snapshot));
  function summary(state, now = Date.now()) {
    const accounts = state.accounts || [], p = state.policy;
    const enabled = accounts.filter(s => !s.account.disabled);
    const protectedAccounts = enabled.filter(s => ruleFor(p, s.account)?.enabled);
    const planned = accounts.filter(s => actionable(s) && validDate(s.reset?.next_at) && liveCredits(s, now).length)
      .sort((a,b) => Date.parse(a.reset.next_at) - Date.parse(b.reset.next_at));
    return {total:accounts.length, enabled:enabled.length, disabled:accounts.length-enabled.length,
      rules:p.rules.filter(r => r.enabled).length, effectiveRules:protectedAccounts.length,
      held:enabled.filter(s => s.would_block).length,
      cards:accounts.reduce((n,s) => n + liveCredits(s,now).length,0),
      automaticCards:accounts.filter(actionable).reduce((n,s) => n + liveCredits(s,now).length,0),
      blocked:enabled.filter(s => ['blocked','error'].includes(scheduler(s))), next:planned[0]};
  }
  function threshold(p, rule, bucket) {
    const custom = !!Object.keys(rule?.windows || {}).length;
    const selected = custom ? Object.hasOwn(rule.windows,bucket.id) : !bucket.auxiliary;
    const minimum = Number(custom && selected ? rule.windows[bucket.id] : rule?.min_remaining_percent ?? 20);
    const stop = Math.min(100, minimum + Number(p.safety_percent || 0));
    const resume = Math.min(100, stop + Number(p.resume_gap_percent || 0));
    return {selected, protected:!!rule?.enabled && selected, minimum, stop, resume, permanent:!!rule?.enabled && selected && stop >= 100};
  }
  function changes(base, draft, accounts) {
    const fields = [];
    function visit(a,b,path='') {
      if (path === 'revision') return;
      if (JSON.stringify(a) === JSON.stringify(b)) return;
      if (a && b && typeof a === 'object' && typeof b === 'object') {
        for (const key of new Set([...Object.keys(a),...Object.keys(b)])) visit(a[key],b[key],path ? path+'.'+key : key);
      } else fields.push({path,before:a,after:b});
    }
    visit(base,draft);
    const globalChange = fields.some(f => !['rules.','auto_reset.accounts.','auto_activate.accounts.','auto_activate.account_models.'].some(prefix => f.path.startsWith(prefix)));
    const impacted = accounts.filter(s => !s.account.disabled && (globalChange ||
      JSON.stringify(ruleFor(base,s.account)) !== JSON.stringify(ruleFor(draft,s.account)) ||
      autoEnabled(base,s.account) !== autoEnabled(draft,s.account) ||
      activationEnabled(base,s.account) !== activationEnabled(draft,s.account) ||
      activationModelFor(base,s.account) !== activationModelFor(draft,s.account)));
    return {fields,count:fields.length,impacted:impacted.length};
  }
  function groups(events) {
    const result = new Map();
    for (const event of events || []) {
      const key = event.operation_id || `${event.account}\u0000${event.credit || 'legacy'}`;
      if (!result.has(key)) result.set(key,{key,account:event.account,credit:event.credit,events:[]});
      result.get(key).events.push(event);
    }
    return [...result.values()].map(g => { g.events.sort((a,b) => Date.parse(a.at)-Date.parse(b.at)); g.latest=g.events.at(-1); return g; })
      .sort((a,b) => Date.parse(b.latest.at)-Date.parse(a.latest.at));
  }
  function activationGroups(events) {
    const result = new Map();
    for (const [index,event] of (events || []).entries()) {
      const key = event.operation_id || event.id || `${event.account}\u0000${event.at}\u0000${index}`;
      if (!result.has(key)) result.set(key,{key,account:event.account || '',events:[]});
      result.get(key).events.push(event);
    }
    return [...result.values()].map(g => {g.events.sort((a,b) => Date.parse(a.at)-Date.parse(b.at));g.latest=g.events.at(-1);return g;})
      .sort((a,b) => Date.parse(b.latest.at)-Date.parse(a.latest.at));
  }
  function accountMatches(snapshot, query, filter, p) {
    const a = snapshot.account, h=health(snapshot,p);
    if (query && ![a.name,a.id,a.provider,a.email].join(' ').toLowerCase().includes(query.toLowerCase())) return false;
    return filter === 'all' || filter === 'enabled' && !a.disabled || filter === 'disabled' && a.disabled ||
      filter === 'protected' && snapshot.would_block || filter === 'attention' && !a.disabled && (['error','stale','unknown'].includes(h) || ['blocked','error'].includes(scheduler(snapshot)) || activationAttention(snapshot));
  }
  globalThis.QuotaUI = {copy,validDate,keys,policy,ruleFor,autoEnabled,activationEnabled,activationModelFor,activationAttention,activationWindow,health,liveCredits,scheduler,actionable,summary,threshold,changes,groups,activationGroups,accountMatches};
})();

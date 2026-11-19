import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';
const context={};vm.runInNewContext(readFileSync(new URL('../web/ui-model.js',import.meta.url),'utf8'),context);
const M=context.QuotaUI;
const now=Date.parse('2026-11-09T08:00:00Z'),future='2026-12-02T00:00:00Z';
const p=M.policy({revision:3,mode:'enforce',max_age_seconds:120,safety_percent:3,resume_gap_percent:5,rules:[{account:'a.json',enabled:false,min_remaining_percent:20}],auto_reset:{enabled:true,before_hours:6,include_new_accounts:false,accounts:{'a.json':true}}});
const active={account:{id:'a',name:'a.json',provider:'codex',disabled:false},health:'healthy',reset:{enabled:true,scheduler_state:'waiting',error:'old failure',next_at:'2026-12-01T18:00:00Z',last_result:{at:'2026-10-30T00:00:00Z',error:'old failure'},credits:[{id:'one',expires_at:future}]}};
const disabled={account:{id:'b',name:'b.json',provider:'codex',disabled:true},reset:{enabled:false,scheduler_state:'disabled',credits:[{id:'two',expires_at:future}]}};
test('summary distinguishes registered, enabled, effective rules and automatic cards; history never hides a valid schedule',()=>{
 const s=M.summary({policy:p,accounts:[active,disabled]},now);
 assert.deepEqual([s.total,s.enabled,s.disabled,s.rules,s.effectiveRules,s.cards,s.automaticCards],[2,1,1,0,0,2,1]);assert.equal(s.next.account.id,'a');
});
test('blocked scheduling is excluded independently from historical redemption diagnostics',()=>{
 const blocked=structuredClone(active);blocked.reset.scheduler_state='blocked';
 const s=M.summary({policy:p,accounts:[blocked]},now);assert.equal(s.next,undefined);assert.equal(s.automaticCards,0);assert.equal(s.blocked.length,1);
});
test('new accounts require explicit opt in unless include_new_accounts is enabled',()=>{
 assert.equal(M.autoEnabled(p,active.account),true);assert.equal(M.autoEnabled(p,disabled.account),false);
 const migrated=M.copy(p);migrated.auto_reset.include_new_accounts=true;assert.equal(M.autoEnabled(migrated,disabled.account),true);
});
test('unknown, stale and failed queries never become healthy when unknown_action allows calls',()=>{
 const unknown={account:active.account,checked_at:'0001-01-01T00:00:00Z'};
 assert.equal(M.health(unknown,{...p,unknown_action:'allow'},now),'unknown');assert.equal(M.health({...unknown,checked_at:'2026-11-09T00:00:00Z'},p,now),'stale');assert.equal(M.health({...unknown,error:'network'},p,now),'error');
});
test('threshold preview matches selected windows, auxiliary exclusions and clamped permanent stop',()=>{
 const r={enabled:true,min_remaining_percent:20,windows:{weekly:97}};
 assert.equal(M.threshold(p,r,{id:'daily'}).protected,false);assert.equal(M.threshold(p,r,{id:'weekly'}).permanent,true);assert.equal(M.threshold(p,r,{id:'weekly'}).stop,100);
 assert.equal(M.threshold(p,{enabled:true,min_remaining_percent:20},{id:'aux',auxiliary:true}).protected,false);
 assert.deepEqual(JSON.parse(JSON.stringify(M.threshold(p,{enabled:true,min_remaining_percent:20},{id:'daily'}))),{selected:true,protected:true,minimum:20,stop:23,resume:28,permanent:false});
});
test('draft change count ignores revision and reports impacted enabled accounts',()=>{
 const draft=M.copy(p);draft.revision=100;assert.equal(M.changes(p,draft,[active,disabled]).count,0);
 draft.rules[0].min_remaining_percent=30;const delta=M.changes(p,draft,[active,disabled]);assert.equal(delta.count,1);assert.equal(delta.impacted,1);
});
test('operation grouping preserves a chronological timeline and separates operation IDs',()=>{
 const rows=[{operation_id:'one',account:'a.json',at:'2026-11-09T00:01:00Z',state:'success'},{operation_id:'one',account:'a.json',at:'2026-11-09T00:00:00Z',state:'pending'},{operation_id:'two',account:'a.json',at:'2026-11-09T00:02:00Z',state:'pending'}];
 const groups=M.groups(rows);assert.equal(groups.length,2);assert.equal(groups[0].key,'two');assert.equal(groups[1].events[0].state,'pending');assert.equal(groups[1].latest.state,'success');
});
test('older policies receive disabled activation defaults without changing reset settings',()=>{
 const old={revision:4,auto_reset:{enabled:true,accounts:{'a.json':true}}};const normalized=M.policy(old);
 assert.deepEqual(JSON.parse(JSON.stringify(normalized.auto_activate)),{enabled:false,include_new_accounts:false,natural:true,reset:true,five_hour:true,weekly:true,model:'',daily_times:[],accounts:{},account_models:{}});
 assert.equal(normalized.auto_reset.enabled,true);assert.equal(old.auto_activate,undefined);
 const partial=M.policy({auto_activate:{natural:false,weekly:false,model:'test-mini'}});assert.equal(partial.auto_activate.natural,false);assert.equal(partial.auto_activate.weekly,false);assert.equal(partial.auto_activate.enabled,false);
});
test('activation account selection and models resolve account aliases and inheritance independently',()=>{
 const selected=M.copy(p);selected.auto_activate={enabled:true,include_new_accounts:true,natural:true,reset:true,five_hour:true,weekly:true,model:'global-model',accounts:{a:false},account_models:{'a.json':'account-model'}};
 assert.equal(M.activationEnabled(selected,active.account),false);assert.equal(M.activationEnabled(selected,disabled.account),true);
 assert.equal(M.activationModelFor(selected,active.account),'account-model');assert.equal(M.activationModelFor(selected,disabled.account),'global-model');selected.auto_activate.account_models['a.json']='';assert.equal(M.activationModelFor(selected,active.account),'global-model');
 assert.equal(M.autoEnabled(selected,active.account),true);
});
test('activation edits count only accounts whose effective setting changes',()=>{
 const original=M.copy(p),edited=M.copy(p),other={account:{id:'c',name:'c.json',provider:'codex',disabled:false}};
 edited.auto_activate.accounts['a.json']=true;let delta=M.changes(original,edited,[active,disabled,other]);assert.equal(delta.count,1);assert.equal(delta.impacted,1);
 edited.auto_activate.account_models['c.json']='compact-model';delta=M.changes(original,edited,[active,disabled,other]);assert.equal(delta.count,2);assert.equal(delta.impacted,2);
 edited.auto_activate.enabled=true;delta=M.changes(original,edited,[active,disabled,other]);assert.equal(delta.impacted,2);
});
test('uncertain activation participates in attention filters even with healthy quota',()=>{
 const uncertain={...active,health:'healthy',activation:{state:'uncertain',last_result:{error:'request outcome unknown'}}};
 assert.equal(M.accountMatches(uncertain,'','attention',p),true);assert.equal(M.accountMatches({...uncertain,account:{...uncertain.account,disabled:true}},'','attention',p),false);
 assert.equal(M.activationWindow('five_hour'),'五小时额度');assert.equal(M.activationWindow('weekly'),'周额度');assert.equal(M.activationWindow('primary'),'primary');assert.equal(M.activationWindow('secondary'),'secondary');
});
test('activation timelines group task IDs and retain confirmation separately from successful calls',()=>{
 const rows=[{id:'one',account:'a.json',at:'2026-11-16T00:01:00Z',state:'succeeded',confirmed:false},{id:'one',account:'a.json',at:'2026-11-16T00:02:00Z',state:'succeeded',confirmed:true},{id:'two',account:'a.json',at:'2026-11-16T00:03:00Z',state:'uncertain'}];
 const groups=M.activationGroups(rows);assert.equal(groups.length,2);assert.equal(groups[0].latest.state,'uncertain');assert.equal(groups[1].events.length,2);assert.equal(groups[1].events[0].confirmed,false);assert.equal(groups[1].latest.confirmed,true);
});

test('daily schedule accepts multiple Beijing times and preserves validation errors',()=>{
 const times=M.dailyTimes(' 19:00， 00:00,16:00 ');
 assert.equal(JSON.stringify(times),JSON.stringify(['00:00','16:00','19:00']));
 assert.equal(M.dailyTimesError(times),'');
 for(const input of ['9:00','24:01','10:75','11:00,','11:00,11:00']) assert.notEqual(M.dailyTimesError(M.dailyTimes(input)),'');
 assert.equal(JSON.stringify(M.dailyTimes('   ')),'[]');
 assert.equal(M.dailyTimesError(M.dailyTimes('00:00,23:59')),'');
 const edited=M.policy({...p,auto_activate:{...p.auto_activate,daily_times:times}});
 assert.equal(M.changes(p,edited,[active]).impacted,1);
});

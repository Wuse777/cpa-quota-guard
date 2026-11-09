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

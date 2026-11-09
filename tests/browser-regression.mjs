/** Actual Chromium checks against synthetic CPAMC messages, plus optional real isolated CPA smoke. */
import assert from 'node:assert/strict';
import {readFileSync,existsSync,mkdirSync,writeFileSync} from 'node:fs';
import {createServer} from 'node:http';
import {resolve,dirname} from 'node:path';
import {fileURLToPath} from 'node:url';
import {chromium} from 'playwright';
const root=resolve(dirname(fileURLToPath(import.meta.url)),'..'),output=resolve(root,'test-results/browser');mkdirSync(output,{recursive:true});
const args=process.argv.slice(2),arg=name=>{const i=args.indexOf(name);return i<0?undefined:args[i+1];};
const at=hours=>new Date(Date.now()+hours*3600000).toISOString();
const p={revision:10,mode:'enforce',poll_seconds:60,max_age_seconds:180,timeout_seconds:10,safety_percent:3,resume_gap_percent:5,unknown_action:'allow',strategy:'round-robin',rules:[{account:'a.json',enabled:false,min_remaining_percent:20},{account:'b.json',enabled:false,min_remaining_percent:20}],auto_reset:{enabled:true,before_hours:6,include_new_accounts:false,accounts:{'a.json':true}}};
const fixture={version:'0.3.0',policy:p,accounts:[
 {account:{id:'a',name:'a.json',provider:'codex',disabled:false},health:'healthy',checked_at:at(-.01),attempt_at:at(-.01),buckets:[{id:'weekly',label:'每周额度',remaining_percent:52,reset_at:at(60)},{id:'daily',label:'短期额度',remaining_percent:73,reset_at:at(5)}],would_block:false,reason:'未启用额度保护',reset:{enabled:true,scheduler_state:'waiting',message:'等待进入到期前自动使用窗口',next_at:at(30),checked_at:at(-.01),error:'历史兑换未确认成功；旧版本未保存 HTTP 状态及上游错误',last_result:{at:at(-240),error:'历史兑换未确认成功；旧版本未保存 HTTP 状态及上游错误'},credits:[{id:'credit-one',expires_at:at(36)}]}},
 {account:{id:'b',name:'b.json',provider:'codex',disabled:true},health:'disabled',checked_at:at(-200),attempt_at:at(-200),buckets:[{id:'weekly',label:'每周额度',remaining_percent:90}],would_block:false,reason:'未启用额度保护',reset:{enabled:false,scheduler_state:'disabled',message:'账号已停用',checked_at:at(-200),credits:[{id:'credit-two',expires_at:at(50)}]}},
 {account:{id:'g',name:'g.json',provider:'antigravity',disabled:true},health:'disabled',checked_at:at(-200),buckets:[],would_block:false,reason:'未启用额度保护'}],
 reset_events:[{operation_id:'one',account:'a.json',credit:'credit-old',at:at(-240),state:'pending',message:'开始核实',result:{http_status:502,code:'upstream_error',error:'虚构上游错误'}},{operation_id:'one',account:'a.json',credit:'credit-old',at:at(-239),state:'expired',message:'目标卡到期；保留历史错误',result:{http_status:502,error:'虚构上游错误'}},{operation_id:'two',account:'b.json',credit:'credit-done',at:at(-250),state:'success',message:'目标卡已核实'}],
 build:{version:'0.3.0',commit:'fixture0123',built_at:at(-1),go_version:'go1.23.12',bridge_version:'0.3.0',cpa_compatibility:'8.0.35 / C ABI 1 / schema 6',cpamc_compatibility:'1.26.5 (verified HTML build)'},health:{last_quota_success_at:at(-.01),last_reset_success_at:at(-.01)},notifications:{enabled:false,health:'disabled'}};
const light={'--bg-primary':'#f0eee8','--bg-secondary':'#faf9f5','--bg-tertiary':'#e9e6df','--text-primary':'#2d2a26','--text-secondary':'#6d6760','--text-tertiary':'#77716b','--border-color':'#dedbd4','--primary-color':'#8b8680'};
const dark={...light,'--bg-primary':'#1d1b18','--bg-secondary':'#141310','--bg-tertiary':'#302d27','--bg-hover':'#292620','--text-primary':'#eeece6','--text-secondary':'#bbb5aa','--text-tertiary':'#aaa398','--border-color':'#3b3831','--border-hover':'#575147','--success-badge-bg':'#123f30','--success-badge-text':'#9bebc5','--success-badge-border':'#285e49','--failure-badge-bg':'#482c25','--failure-badge-text':'#ffb4a4','--failure-badge-border':'#805044','--error-color':'#ffab99'};
const panel=`<!doctype html><html><head><meta charset="utf-8"><style>body{margin:0;background:#faf9f5}header{box-sizing:border-box;position:absolute;inset:0 0 auto;height:54px;padding:14px 24px;border-bottom:1px solid #dedbd4;font:600 14px system-ui;color:#6d6760}header span{float:right;font-weight:400;font-size:12px}iframe{border:0;width:100%;height:100vh;display:block}</style></head><body><header>CPAMC <span>隔离浏览器验收 · 虚构账号</span></header><iframe title="账号额度保护" src="/plugin"></iframe><script>
window.fixture=${JSON.stringify(fixture)};window.behavior={delay:0,conflict:false};window.calls={status:0,policy:0,refresh:0};window.theme='light';
window.setTheme=theme=>{window.theme=theme;document.documentElement.dataset.theme=theme;document.body.style.background=theme==='dark'?'#141310':'#faf9f5';document.querySelector('header').style.color=theme==='dark'?'#bbb5aa':'#6d6760';document.querySelector('iframe').contentWindow.postMessage({channel:'quota-guard:cpamc-session:v1',type:'appearance',data:{theme,colors:theme==='dark'?${JSON.stringify(dark)}:${JSON.stringify(light)},layout:{inline:'24px',top:'78px',bottom:'24px'}}},location.origin);};
window.addEventListener('message',async e=>{if(e.source!==document.querySelector('iframe').contentWindow||e.origin!==location.origin||e.data?.channel!=='quota-guard:cpamc-session:v1')return;const q=e.data;if(q.type!=='request')return;let status=200,data;if(q.operation==='appearance'){window.setTheme(window.theme);data={theme:window.theme,colors:window.theme==='dark'?${JSON.stringify(dark)}:${JSON.stringify(light)}};}else{window.calls[q.operation]=(window.calls[q.operation]||0)+1;if(q.operation==='policy'){await new Promise(r=>setTimeout(r,window.behavior.delay));if(window.behavior.conflict){window.fixture.policy.revision++;status=409;data={error:'配置已被其他页面修改'};}else{window.fixture.policy=structuredClone(q.body);window.fixture.policy.revision++;data=structuredClone(window.fixture);}}else data=structuredClone(window.fixture);}e.source.postMessage({channel:q.channel,type:'response',id:q.id,status,data},location.origin);});
</script></body></html>`;
const pageHTML=readFileSync(resolve(root,'web/index.html'),'utf8').replace('/* QUOTA_STYLE */',readFileSync(resolve(root,'web/style.css'),'utf8')).replace('/* QUOTA_SCRIPT */',readFileSync(resolve(root,'web/ui-model.js'),'utf8')+'\n'+readFileSync(resolve(root,'web/app.js'),'utf8'));
const server=createServer((req,res)=>{res.setHeader('Content-Type','text/html; charset=utf-8');res.end(req.url==='/plugin'?pageHTML:panel);});await new Promise(r=>server.listen(0,'127.0.0.1',r));
const address=`http://127.0.0.1:${server.address().port}`;
const options={headless:true};if(process.env.PLAYWRIGHT_EXECUTABLE_PATH)options.executablePath=process.env.PLAYWRIGHT_EXECUTABLE_PATH;else if(process.platform==='win32'&&existsSync('C:/Program Files/Google/Chrome/Application/chrome.exe'))options.executablePath='C:/Program Files/Google/Chrome/Application/chrome.exe';
const browser=await chromium.launch(options),checks=[],errors=[];
try {
 const page=await browser.newPage({viewport:{width:1280,height:900},reducedMotion:'reduce'});page.on('pageerror',e=>{errors.push(e.message);console.error('PAGE ERROR',e.message);});
 await page.addInitScript(()=>{const interval=window.setInterval;window.setInterval=(fn,ms,...rest)=>interval(fn,ms===15000?250:ms,...rest);});
 await page.goto(address);const frame=page.frameLocator('iframe');await frame.locator('#content').waitFor({state:'visible'});
 const check=async(name,fn)=>{await fn();checks.push(name);console.log('PASS '+name);};
 const text=selector=>frame.locator(selector).innerText();
 await check('truthful overview, disabled account collapse and historical error retain next schedule',async()=>{
   assert.equal(await text('#account-count'),'3');assert.match(await text('#account-breakdown'),/启用 1 · 停用 2/);assert.match(await text('#rule-count'),/生效规则 0 \/ 0/);assert.equal(await text('#credit-count'),'2');assert.match(await text('#auto-label'),/可自动处理 1 张/);assert.notEqual(await text('#next-countdown'),'—');assert.match(await text('#history-warning'),/上次兑换异常/);assert.equal(await frame.locator('[data-account-id="b"]').getAttribute('open'),null);
 });
 await check('draft editing keeps live refresh, focused input and typed value',async()=>{
   const input=frame.locator('[data-account-id="a"] [data-edit="minimum"]');await input.fill('31');await input.focus();const calls=await page.evaluate(()=>window.calls.status);await page.evaluate(()=>{window.fixture.accounts[0].buckets[0].remaining_percent=47;});await frame.locator('[data-account-id="a"] .bucket-line strong').first().filter({hasText:'47.0%'}).waitFor();assert.equal(await input.inputValue(),'31');assert.equal(await input.evaluate(n=>n===document.activeElement),true);assert.ok(await page.evaluate(()=>window.calls.status)>calls);assert.match(await text('#dirty'),/未保存修改/);
 });
 await check('reload confirmation preserves cancelled drafts and discards only after confirm',async()=>{
   await frame.locator('#reload').click();await frame.locator('#discard-dialog').waitFor({state:'visible'});await frame.locator('#cancel-discard').click();assert.equal(await frame.locator('[data-edit="minimum"]').first().inputValue(),'31');await frame.locator('#discard').click();await frame.locator('#confirm-discard').click();await frame.locator('#savebar').waitFor({state:'hidden'});assert.equal(await frame.locator('[data-edit="minimum"]').first().inputValue(),'20');
 });
 await check('threshold preview, selected window markers and permanent stop warning',async()=>{
   await frame.locator('[data-account-id="a"] [data-edit="guard"]').locator('..').click();await frame.locator('[data-account-id="a"] [data-edit="minimum"]').fill('97');assert.match(await frame.locator('[data-account-id="a"] .threshold-warning').first().innerText(),/100%/);assert.equal(await frame.locator('[data-account-id="a"] .stop-marker').count(),2);await frame.locator('#discard').click();await frame.locator('#confirm-discard').click();await frame.locator('#savebar').waitFor({state:'hidden'});
 });
 await check('saving freezes editors and prevents duplicate submission or swallowed edits',async()=>{
   await page.evaluate(()=>{window.behavior.delay=700;});await frame.locator('[data-account-id="a"] [data-edit="minimum"]').fill('29');await frame.locator('#save-bottom').click();assert.equal(await frame.locator('[data-account-id="a"] [data-edit="minimum"]').isDisabled(),true);assert.equal(await frame.locator('#save-bottom').isDisabled(),true);await frame.locator('#savebar').waitFor({state:'hidden'});assert.equal(await frame.locator('[data-edit="minimum"]').first().inputValue(),'29');assert.equal(await page.evaluate(()=>window.calls.policy),1);await page.evaluate(()=>{window.behavior.delay=0;});
 });
 await check('409 conflict retains draft and exposes server revision conflict',async()=>{
   await page.evaluate(()=>{window.behavior.conflict=true;});await frame.locator('[data-account-id="a"] [data-edit="minimum"]').fill('35');await frame.locator('#save-bottom').click();await frame.locator('#message').filter({hasText:'草稿已保留'}).waitFor();assert.equal(await frame.locator('[data-edit="minimum"]').first().inputValue(),'35');await frame.locator('#conflict').waitFor({state:'visible'});await page.evaluate(()=>{window.behavior.conflict=false;});await frame.locator('#discard').click();await frame.locator('#confirm-discard').click();await frame.locator('#savebar').waitFor({state:'hidden'});
 });
 await check('query health remains explicit under allow unknown policy',async()=>{
   await page.evaluate(()=>{window.fixture.accounts[0].health='unknown';window.fixture.policy.rules[0].enabled=true;});await frame.locator('[data-account-id="a"] .badge').filter({hasText:'额度未知'}).waitFor();assert.match(await text('[data-account-id="a"] .account-badges'),/额度待核实/);assert.doesNotMatch(await text('[data-account-id="a"] .account-badges'),/额度充足/);await page.evaluate(()=>{window.fixture.accounts[0].health='healthy';window.fixture.policy.rules[0].enabled=false;});
 });
 await check('account search and status filters',async()=>{
   await frame.locator('#account-search').fill('b.json');assert.equal(await frame.locator('.account').count(),1);await frame.locator('#account-search').fill('');await frame.locator('#account-filter').selectOption('enabled');assert.equal(await frame.locator('.account').count(),1);await frame.locator('#account-filter').selectOption('all');
 });
 await check('operation grouping, expandable timelines and result filters',async()=>{
   await frame.locator('#events-tab').click();assert.equal(await frame.locator('.operation').count(),2);await frame.locator('.operation').first().locator('summary').click();assert.equal(await frame.locator('.operation').first().locator('.timeline li').count(),2);await frame.locator('#event-filter').selectOption('success');assert.equal(await frame.locator('.operation').count(),1);await frame.locator('#event-filter').selectOption('all');await frame.locator('#accounts-tab').click();
 });
 await check('keyboard tabs, build compatibility and optional notification status',async()=>{
   await frame.locator('#accounts-tab').focus();await page.keyboard.press('ArrowRight');assert.equal(await frame.locator('#settings-tab').getAttribute('aria-selected'),'true');assert.match(await text('#build-info'),/fixture0123/);assert.match(await text('#notification-status'),/尚未启用/);await frame.locator('#accounts-tab').click();
 });
 await check('light, dark, and narrow layouts render without horizontal overflow',async()=>{
   await page.reload();await frame.locator('#content').waitFor({state:'visible'});await page.evaluate(()=>window.setTheme('light'));await page.screenshot({path:resolve(output,'light.png'),fullPage:true});
   await page.evaluate(()=>window.setTheme('dark'));await frame.locator('html[data-theme="dark"]').waitFor();await page.screenshot({path:resolve(output,'dark.png'),fullPage:true});
   await page.setViewportSize({width:390,height:844});assert.ok(await frame.locator('body').evaluate(n=>n.scrollWidth<=window.innerWidth));await page.screenshot({path:resolve(output,'mobile.png'),fullPage:true});
   const firstAccount=await frame.locator('.account').first().boundingBox();assert.ok(firstAccount.y<844,'compact overview should expose the first account on a mobile screen');
 });
 assert.deepEqual(errors,[],'browser console page errors');
 if(arg('--url')) {
   assert.ok(['127.0.0.1','localhost','[::1]'].includes(new URL(arg('--url')).hostname),'The write-enabled CPA smoke accepts only a loopback isolated fixture URL');
   const live=await browser.newPage({viewport:{width:1280,height:900},reducedMotion:'reduce'}),liveErrors=[];live.on('pageerror',e=>liveErrors.push(e.message));await live.goto(arg('--url'));
   const iframe=()=>live.locator('iframe[src*="quota-guard"]');
   await live.locator('input[type="password"], iframe[src*="quota-guard"], a[href*="quota-guard"]').first().waitFor({state:'attached'});
   if(!await iframe().count()){
     const password=live.locator('input[type=password]').first();if(await password.count()){await password.fill(arg('--key')||process.env.QUOTA_GUARD_TEST_KEY||'quota-guard-integration-only');const submit=live.getByRole('button',{name:/登录|连接|Connect|Login|Sign in/i});await submit.last().click();}
     await live.getByText('账号额度保护',{exact:true}).last().click();
   }
   await iframe().waitFor();const embedded=live.frameLocator('iframe[src*="quota-guard"]');await embedded.locator('#content').waitFor({state:'visible'});await embedded.locator('#settings-tab').click();const mode=embedded.locator('[data-field=mode]'),old=await mode.inputValue();await mode.selectOption(old==='observe'?'enforce':'observe');await embedded.locator('#save-bottom').click();await embedded.locator('#message').filter({hasText:'设置已保存'}).waitFor();await mode.selectOption(old);await embedded.locator('#save-bottom').click();await embedded.locator('#savebar').waitFor({state:'hidden'});await embedded.locator('#probe').click();
   // Drive the real parent's theme source; its installed bridge must propagate it.
   await live.evaluate(()=>document.documentElement.setAttribute('data-theme','dark'));await embedded.locator('html[data-theme=dark]').waitFor();await live.setViewportSize({width:390,height:844});assert.ok(await embedded.locator('body').evaluate(n=>n.scrollWidth<=window.innerWidth));await live.screenshot({path:resolve(output,'isolated-cpa-mobile.png'),fullPage:true});assert.deepEqual(liveErrors,[]);checks.push('real isolated CPA login, bridge read/save/refresh, theme and mobile smoke');console.log('PASS '+checks.at(-1));await live.close();
 }
 writeFileSync(resolve(output,'report.json'),JSON.stringify({passed:checks.length,checks},null,2));
 if(args.includes('--update-screenshots'))for(const name of ['light','dark','mobile'])writeFileSync(resolve(root,`docs/images/${name}.png`),readFileSync(resolve(output,`${name}.png`)));
 console.log(`Browser checks passed: ${checks.length}`);
}finally{await browser.close();await new Promise(r=>server.close(r));}

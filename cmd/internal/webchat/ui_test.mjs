// Dependency-free browser regression test. Uses only mock APIs, never real sessions.
// CHROME_BIN=/path/to/chrome node --test cmd/internal/webchat/ui_test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { once } from 'node:events';

const chrome = process.env.CHROME_BIN || [
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/usr/bin/chromium', '/usr/bin/chromium-browser', '/usr/bin/google-chrome',
].find(existsSync);

const fixture = `
Object.defineProperty(window, 'localStorage', { value: {
  getItem(k) { return this[k] ?? null; }, setItem(k,v) { this[k] = String(v); }, removeItem(k) { delete this[k]; }
}});
window.EventSource = class extends EventTarget {
  constructor() { super(); queueMicrotask(() => this.dispatchEvent(new Event('open'))); }
  close() {}
};
window.requests = []; window.apiDelay = 0; window.failNext = false;
window.fixture = { projects: [{ id:'project', name:'Дэманстрацыйны праект', path:'/projects/demo', sessions:[
  {id:'first',title:'Праверка паслядоўнасці web UI з доўгай назвай размовы',state:'working',updated:'2026-05-01T12:00:00Z'},
  {id:'second',title:'Другая размова',state:'saved',updated:'2026-05-01T11:00:00Z'}
]}], pinned:[], warnings:[] };
window.autoTitles = Object.fromEntries(fixture.projects[0].sessions.map(s => [s.id,s.title]));
window.fixtureStatus = {state:'working',operations:[],context:{EstimatedTokens:2048}};
window.fetch = async (url, options = {}) => {
  const body = options.body && JSON.parse(options.body);
  if (options.method) {
    requests.push({url,body,method:options.method});
    if (apiDelay) await new Promise(r => setTimeout(r,apiDelay));
    if (window.holdRename && url.endsWith('/rename')) await new Promise(r => { window.releaseRename = r; });
    if (failNext) { failNext = false; throw new TypeError('offline'); }
    if (url.endsWith('/stop')) fixtureStatus.state = 'stopped';
    if (url.endsWith('/resume')) fixtureStatus.state = 'working';
    if (url.endsWith('/rename')) {
      const session = fixture.projects[0].sessions.find(s => url.endsWith('/'+s.id+'/rename'));
      if (!session) return {ok:false,status:404,text:async()=> 'conversation no longer exists'};
      session.title = body.title.trim().replace(/\\s+/gu,' ') || autoTitles[session.id];
      return {ok:true,json:async()=>({renamed:true})};
    }
    if (options.method === 'DELETE') fixture.projects[0].sessions = fixture.projects[0].sessions.filter(s => !url.endsWith('/'+s.id));
    return {ok:true,json:async()=>({deleted:true})};
  }
  return {ok:true,json:async()=>url.endsWith('navigation') ? structuredClone(fixture) : url.includes('/sessions/') ? {
    items:[{kind:'user',id:'u1',text:'Праверым выгляд і паводзіны інтэрфейсу.'},{kind:'assistant',id:'a1',html:'<p>Гісторыя застаецца пасля вызвалення рэсурсаў.</p><pre><code>go test ./cmd/internal/webchat</code></pre>'}],
    after:2,more:false,status:structuredClone(fixtureStatus)
  } : {}};
};
`;

test('Belarusian web UI: desktop/mobile, focus, actions and drafts', { skip: !chrome && 'Install Chrome/Chromium or set CHROME_BIN', timeout: 90000 }, async t => {
  const dir = mkdtempSync(join(tmpdir(), 'unreal-ui-test-'));
  const browser = spawn(chrome, ['--headless', '--no-first-run', '--disable-background-networking', '--disable-gpu', '--remote-debugging-pipe', '--user-data-dir='+join(dir,'profile')], {stdio:['ignore','ignore','ignore','pipe','pipe']});
  const exited = once(browser, 'exit');
  let sequence = 0, buffer = '';
  const pending = new Map();
  const failPending = error => { for (const p of pending.values()) { clearTimeout(p.timer); p.reject(error); } pending.clear(); };
  browser.on('error', failPending);
  browser.on('exit', () => failPending(new Error('Browser exited')));
  browser.stdio[4].on('data', chunk => {
    buffer += chunk;
    let end;
    while ((end = buffer.indexOf('\0')) >= 0) {
      const message = JSON.parse(buffer.slice(0,end)); buffer = buffer.slice(end+1);
      const p = pending.get(message.id);
      if (p) { pending.delete(message.id); clearTimeout(p.timer); message.error ? p.reject(new Error(JSON.stringify(message.error))) : p.resolve(message.result); }
    }
  });
  const call = (method, params = {}, sessionId) => new Promise((resolve,reject) => {
    const id = ++sequence;
    const timer = setTimeout(() => { pending.delete(id); reject(new Error('CDP timeout: '+method)); }, 10000);
    pending.set(id,{resolve,reject,timer});
    browser.stdio[3].write(JSON.stringify({id,method,params,...(sessionId?{sessionId}:{})})+'\0');
  });
  try {
    const html = readFileSync(new URL('./static/index.html',import.meta.url),'utf8')
      .replace(/<link[^>]*>/g,'').replace(/<script[^>]*><\/script>/g,'')
      .replace('</head>','<style>'+readFileSync(new URL('./static/app.css',import.meta.url),'utf8')+'</style></head>');
    const js = readFileSync(new URL('./static/app.js',import.meta.url),'utf8');
    for (const width of [1280,390,320]) {
      await t.test(width+'px', async () => {
        const {targetId} = await call('Target.createTarget',{url:'about:blank'});
        const {sessionId} = await call('Target.attachToTarget',{targetId,flatten:true});
        const run = (method,params={}) => call(method,params,sessionId);
        const evaluate = async expression => {
          const result = await run('Runtime.evaluate',{expression,awaitPromise:true,returnByValue:true});
          if (result.exceptionDetails) throw new Error(JSON.stringify(result.exceptionDetails));
          return result.result.value;
        };
        const wait = expression => evaluate(`(async()=>{for(let i=0;i<200;i++){if(${expression})return true;await new Promise(r=>setTimeout(r,10));}throw new Error('Condition timed out');})()`);
        const key = async (key, modifiers=0) => {
          const windowsVirtualKeyCode = {Tab:9,Escape:27,Enter:13,' ':32}[key];
          const text = key==='Enter' ? '\r' : key===' ' ? ' ' : undefined;
          await run('Input.dispatchKeyEvent',{type:'keyDown',key,modifiers,windowsVirtualKeyCode,text});
          await run('Input.dispatchKeyEvent',{type:'keyUp',key,modifiers,windowsVirtualKeyCode});
        };
        const pointAt = id => evaluate(`{const e=document.getElementById(${JSON.stringify(id)}),r=e.getBoundingClientRect();if(!r.width||!r.height)throw new Error('Hidden click target: '+e.id);({x:r.x+r.width/2,y:r.y+r.height/2})}`);
        const click = async id => {
          const point = await pointAt(id);
          await run('Input.dispatchMouseEvent',{type:'mouseMoved',...point});
          await run('Input.dispatchMouseEvent',{type:'mousePressed',button:'left',clickCount:1,...point});
          await run('Input.dispatchMouseEvent',{type:'mouseReleased',button:'left',clickCount:1,...point});
        };
        const tap = async id => {
          const point = await pointAt(id);
          await run('Input.dispatchTouchEvent',{type:'touchStart',touchPoints:[point]});
          await run('Input.dispatchTouchEvent',{type:'touchEnd',touchPoints:[]});
        };
        const fits = async () => assert.equal(await evaluate(`document.documentElement.scrollWidth <= innerWidth && [...document.querySelectorAll('dialog[open]')].every(e=>e.scrollWidth<=e.clientWidth)`),true,'horizontal overflow');
        await run('Page.enable');
        await run('Emulation.setDeviceMetricsOverride',{width,height:844,deviceScaleFactor:1,mobile:false});
        const {frameTree} = await run('Page.getFrameTree');
        await run('Page.setDocumentContent',{frameId:frameTree.frame.id,html});
        await evaluate(fixture);
        // Load a standalone classic script, as production does. Concatenating the
        // fixture with app.js would silently disable app.js's 'use strict'.
        await evaluate(`window.uiErrors=[];window.addEventListener('error',e=>uiErrors.push(e.message));window.addEventListener('unhandledrejection',e=>uiErrors.push(String(e.reason)));const script=document.createElement('script');script.textContent=${JSON.stringify(js)};document.head.append(script);`);
        await wait(`state.session === 'first' && state.status && !state.refreshing`);
        assert.equal(await evaluate(`document.documentElement.lang`),'be');
        assert.equal(await evaluate(`!!document.getElementById('release')`),false);
        assert.deepEqual(await evaluate(`[1,2,5,11,21,22,25].map(conversationCount)`),['1 размова','2 размовы','5 размоў','11 размоў','21 размова','22 размовы','25 размоў']);
        assert.equal(await evaluate(`$('send').disabled`),true,'empty send enabled');
        await fits();

        // Common text button geometry, including dynamically generated controls.
        const styles = await evaluate(`['stop','send','rename-session','confirm-rename','delete-session','confirm-delete'].map(id=>{const s=getComputedStyle($(id));return [s.fontSize,s.fontWeight,s.padding,s.minHeight]})`);
        for (const style of styles) assert.deepEqual(style,['13px','500','8px 12px',width<761?'44px':'36px']);
        assert.deepEqual(await evaluate(`['login-error','project-error','rename-error','delete-error'].map(id=>getComputedStyle($(id)).color)`),Array(4).fill('rgb(240, 167, 157)'));
        await evaluate(`$('message').focus()`);
        assert.equal(await evaluate(`getComputedStyle($('message')).outlineStyle`),'solid');
        const connected = await evaluate(`connection('connected');getComputedStyle($('connection'),'::before').backgroundColor`);
        const reconnecting = await evaluate(`connection('reconnecting');getComputedStyle($('connection'),'::before').backgroundColor`);
        assert.notEqual(connected,reconnecting);
        await evaluate(`connection('connected')`);

        // Use browser input, not element.click(): pointerdown changes focus before
        // click, and can remove the target if the menu closes on focusout.
        await click('toggle-actions');
        await click('rename-session');
        assert.equal(await evaluate(`$('rename-dialog').open`),true,'pointer click must open rename');
        await click('cancel-rename');
        // Safari-style buttons may blur the old control without focusing the
        // pressed button (focusout.relatedTarget is null). Reproduce that order.
        await evaluate(`window.noButtonFocus = event => { if (event.target.closest('#conversation-actions button')) { event.preventDefault(); document.activeElement.blur(); } }; document.addEventListener('mousedown',noButtonFocus,true);`);
        await click('toggle-actions');
        await click('rename-session');
        assert.equal(await evaluate(`$('rename-dialog').open`),true,'focus loss during pointerdown must not swallow rename click');
        await click('cancel-rename');
        await click('toggle-actions');
        await click('delete-session');
        assert.equal(await evaluate(`$('delete-dialog').open`),true,'focus loss must not swallow delete click');
        await click('cancel-delete');
        if (width<761) {
          await run('Emulation.setTouchEmulationEnabled',{enabled:true,maxTouchPoints:1});
          await tap('toggle-actions');
          await tap('rename-session');
          await wait(`$('rename-dialog').open`);
          await tap('cancel-rename');
          await tap('toggle-actions');
          await tap('delete-session');
          await wait(`$('delete-dialog').open`);
          await tap('cancel-delete');
          await run('Emulation.setTouchEmulationEnabled',{enabled:false});
        }
        await evaluate(`document.removeEventListener('mousedown',noButtonFocus,true)`);
        // Unknown focus loss alone is not dismissal, but an outside click is.
        await click('toggle-actions');
        await evaluate(`document.activeElement.blur()`);
        await click('transcript');
        assert.equal(await evaluate(`$('conversation-actions-panel').hidden`),true);
        await click('toggle-actions');
        await key('Tab');
        assert.equal(await evaluate(`document.activeElement.id`),'delete-session');
        await key('Tab');
        assert.equal(await evaluate(`$('conversation-actions-panel').hidden`),true,'Tab outside must dismiss the menu');
        await click('toggle-actions');
        await evaluate(`window.dispatchEvent(new Event('blur'))`);
        assert.equal(await evaluate(`$('conversation-actions-panel').hidden`),true,'window blur must dismiss the menu');
        for (const activation of ['Enter',' ']) {
          await click('toggle-actions');
          await key(activation);
          assert.equal(await evaluate(`$('rename-dialog').open`),true,'keyboard activation must open rename');
          await click('cancel-rename');
        }

        // Disclosure and destructive confirmation remain keyboard reachable.
        await evaluate(`$('toggle-actions').click()`);
        assert.equal(await evaluate(`document.activeElement.id`),'rename-session');
        await key('Escape');
        assert.equal(await evaluate(`document.activeElement.id`),'toggle-actions');
        await evaluate(`$('toggle-actions').click();$('delete-session').click()`);
        assert.equal(await evaluate(`$('delete-dialog').open && document.activeElement.id==='cancel-delete'`),true);
        await fits();
        await evaluate(`$('cancel-delete').click()`);
        assert.equal(await evaluate(`document.activeElement.id`),'toggle-actions');

        // Renaming is a metadata edit: preserve draft/history and target identity.
        await evaluate(`$('message').value='Чарнавік перад перайменаваннем';$('message').dispatchEvent(new Event('input'));$('toggle-actions').click();$('rename-session').click()`);
        assert.equal(await evaluate(`$('rename-title').value === $('title').textContent && document.activeElement.id==='rename-title' && $('rename-title').selectionStart===0 && $('rename-title').selectionEnd===$('rename-title').value.length`),true);
        await fits();
        if (process.env.UI_SCREENSHOTS) {
          const shot = await run('Page.captureScreenshot',{format:'png'});
          writeFileSync(join(process.env.UI_SCREENSHOTS,`rename-${width}.png`),Buffer.from(shot.data,'base64'));
        }
        await evaluate(`$('rename-title').value='🚀'.repeat(201);$('rename-form').requestSubmit()`);
        assert.equal(await evaluate(`$('rename-error').textContent.includes('200') && !state.renaming && !requests.some(r=>r.url.endsWith('/rename'))`),true);
        await key('Escape');
        assert.equal(await evaluate(`!$('rename-dialog').open && document.activeElement.id==='toggle-actions'`),true);
        await evaluate(`window.renameText='Аптымізацыя web UI 🚀';$('toggle-actions').click();$('rename-session').click();$('rename-title').value=renameText;failNext=true;holdRename=true;$('rename-form').requestSubmit();$('rename-form').requestSubmit()`);
        assert.deepEqual(await evaluate(`[$('confirm-rename').disabled,$('rename-title').readOnly,$('confirm-rename').textContent]`),[true,true,'Захоўваем…']);
        await key('Escape');
        assert.deepEqual(await evaluate(`[$('rename-dialog').open,state.renaming]`),[true,true]);
        await evaluate(`holdRename=false;releaseRename()`);
        await wait(`!state.renaming && $('rename-error').textContent`);
        assert.equal(await evaluate(`$('rename-title').value===renameText && requests.filter(r=>r.url.endsWith('/rename')).length===1`),true);
        await evaluate(`$('rename-form').requestSubmit()`);
        await wait(`!state.renaming && !$('rename-dialog').open && $('title').textContent===renameText`);
        assert.equal(await evaluate(`$('message').value==='Чарнавік перад перайменаваннем' && document.querySelectorAll('.message').length===2 && state.status.state==='working'`),true);
        await evaluate(`changeTab('recent')`);
        assert.equal(await evaluate(`document.querySelector('#sessions .row-title').textContent`),'Аптымізацыя web UI 🚀');
        await evaluate(`fixture.pinned=[{project:'project',session:'first'}];refresh()`);
        assert.equal(await evaluate(`document.querySelector('#pinned .row-title').textContent`),'Аптымізацыя web UI 🚀');
        await evaluate(`openFolder('project')`);
        assert.equal(await evaluate(`document.querySelector('#sessions .row-title').textContent`),'Аптымізацыя web UI 🚀');
        await evaluate(`$('toggle-actions').click();$('rename-session').click();$('rename-title').value='Мая яшчэ не захаваная назва';fixture.projects[0].sessions[0].title='<b>Назва з іншага браўзера</b>';refresh()`);
        assert.equal(await evaluate(`$('title').textContent==='<b>Назва з іншага браўзера</b>' && !$('title').querySelector('b') && $('rename-title').value==='Мая яшчэ не захаваная назва'`),true);
        await evaluate(`selectSession('second','project')`);
        await evaluate(`$('rename-form').requestSubmit()`);
        await wait(`!state.renaming && !$('rename-dialog').open`);
        assert.equal(await evaluate(`requests.filter(r=>r.url.endsWith('/rename')).at(-1).url.endsWith('/first/rename') && fixture.projects[0].sessions[1].title===autoTitles.second`),true);
        await evaluate(`selectSession('first','project')`);
        await evaluate(`$('toggle-actions').click();$('rename-session').click();$('rename-title').value='';$('rename-form').requestSubmit()`);
        await wait(`!state.renaming && $('title').textContent===autoTitles.first`);
        await evaluate(`fixture.pinned=[];changeTab('projects');refresh()`);

        if (width<761) {
          await evaluate(`$('open-sidebar').click()`);
          assert.equal(await evaluate(`$('main').inert && document.activeElement.id==='close-sidebar'`),true);
          await key('Tab',8); // Shift+Tab wraps to the last sidebar control.
          assert.equal(await evaluate(`document.activeElement.id`),'logout');
          await key('Tab');
          assert.equal(await evaluate(`document.activeElement.id`),'close-sidebar');
          await evaluate(`$('add-project').click()`);
          await key('Escape');
          assert.equal(await evaluate(`$('app').classList.contains('sidebar-open') && !$('project-dialog').open`),true);
          await key('Escape');
          assert.equal(await evaluate(`!$('main').inert && document.activeElement.id==='open-sidebar'`),true);
        }

        // Busy feedback prevents duplicate stop requests.
        await evaluate(`apiDelay=100; $('stop').click(); $('stop').click()`);
        assert.deepEqual(await evaluate(`[$('stop').disabled,$('stop').textContent]`),[true,'Спыняем…']);
        await wait(`!state.action && state.status.state==='stopped'`);
        assert.equal(await evaluate(`requests.filter(r=>r.url.endsWith('/stop')).length`),1);
        await evaluate(`$('resume').click()`);
        await wait(`!state.action && state.status.state==='working'`);

        // Drafts survive switching; failed sends keep their ID/text for Retry.
        await evaluate(`$('message').value='Чарнавік';$('message').dispatchEvent(new Event('input'));`);
        await evaluate(`selectSession('second','project')`);
        await evaluate(`selectSession('first','project')`);
        assert.equal(await evaluate(`$('message').value`),'Чарнавік');
        const sendRect = await evaluate(`{const r=$('send').getBoundingClientRect();({x:r.x+r.width/2,y:r.y+r.height/2})}`);
        await run('Input.dispatchMouseEvent',{type:'mouseMoved',...sendRect});
        assert.equal(await evaluate(`getComputedStyle($('send')).backgroundColor`),'rgb(215, 233, 205)');
        await run('Input.dispatchMouseEvent',{type:'mouseMoved',x:0,y:0});
        await evaluate(`failNext=true;$('composer').requestSubmit()`);
        await wait(`!state.sending && state.pending`);
        assert.deepEqual(await evaluate(`[$('message').readOnly,$('send').textContent]`),[true,'Паўтарыць ↑']);
        await evaluate(`$('composer').requestSubmit()`);
        await wait(`!state.sending && !state.pending`);
        assert.equal(await evaluate(`{const sends=requests.filter(r=>r.url.endsWith('/send'));JSON.stringify(sends[0].body)===JSON.stringify(sends[1].body)}`),true);

        // Runtime status changes (including auto-release -> saved) keep drafts.
        await evaluate(`$('message').value='Не губляць';$('message').dispatchEvent(new Event('input'));fixtureStatus.state='saved';refresh()`);
        assert.equal(await evaluate(`$('message').value==='Не губляць' && !$('resume').hidden`),true);
        await evaluate(`renderTool({id:'permission',state:'awaiting permission',approval_id:'once',description:'ssh example.invalid',directory:'/projects/demo'})`);
        assert.equal(await evaluate(`document.querySelector('.tool .primary').textContent`),'Дазволіць адзін раз');
        await fits();

        // Capture screenshots only when requested; no golden files or dependencies.
        if (process.env.UI_SCREENSHOTS) {
          const shot = await run('Page.captureScreenshot',{format:'png'});
          writeFileSync(join(process.env.UI_SCREENSHOTS,`web-${width}.png`),Buffer.from(shot.data,'base64'));
        }
        // Confirm only the selected session is deleted, and its draft is cleared.
        await evaluate(`apiDelay=0;$('toggle-actions').click();$('delete-session').click();$('delete-form').requestSubmit()`);
        await wait(`!state.deleting && !state.session`);
        assert.equal(await evaluate(`localStorage.getItem('unreal.draft.project.first')`),null);
        assert.equal(await evaluate(`fixture.projects[0].sessions[0].id`),'second');
        // A deletion from another browser cannot redirect/resurrect a rename.
        await evaluate(`selectSession('second','project')`);
        await evaluate(`$('toggle-actions').click();$('rename-session').click();$('rename-title').value='Не губляць назву';fixture.projects[0].sessions=[];refresh()`);
        await evaluate(`$('rename-form').requestSubmit()`);
        await wait(`!state.renaming && $('rename-error').textContent.includes('выдаленая')`);
        assert.equal(await evaluate(`$('rename-dialog').open && $('rename-title').value==='Не губляць назву' && state.session===''`),true);
        await evaluate(`$('cancel-rename').click()`);
        assert.deepEqual(await evaluate(`uiErrors`),[],'browser console errors');
        await call('Target.closeTarget',{targetId});
      });
    }
  } finally {
    browser.kill(); await exited.catch(()=>{});
    rmSync(dir,{recursive:true,force:true});
  }
});

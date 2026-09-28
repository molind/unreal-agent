'use strict';

const $ = id => document.getElementById(id);
const state = {
  project: '', session: '', projects: [], after: 0,
  generation: 0, compactTurns: new Set(), tools: new Map(), rendered: new Set(),
  source: null, refreshing: false, refreshAgain: false, sending: false, creating: false,
  status: null, pending: null, timer: null,
  navigationRows: new Map(), pins: [], warnings: [],
  tab: saved('tab', 'projects'), folder: saved('folder'),
};

function saved(key, fallback = '') {
  try { return localStorage.getItem('unreal.' + key) || fallback; } catch { return fallback; }
}
function save(key, value) {
  try { if (value === null) localStorage.removeItem('unreal.' + key); else localStorage.setItem('unreal.' + key, value); }
  catch { notice('Browser storage is unavailable. Keep this tab open until your message is confirmed.'); }
}
function draftKey() { return 'draft.' + state.project + '.' + state.session; }
function pendingKey() { return 'pending.' + state.project + '.' + state.session; }
function notice(text = '') { $('notice').textContent = text; $('notice').hidden = !text; }
function sidebar(open) { $('app').classList.toggle('sidebar-open', open); }

function newID() {
  if (crypto.randomUUID) return crypto.randomUUID();
  // Direct HTTP on a Tailscale IP is not a secure browser context.
  // getRandomValues still supplies secure randomness there.
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

async function api(path, body) {
  const options = body === undefined ? {} : { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) };
  const response = await fetch('/api/' + path, { ...options, cache: 'no-store' });
  if (!response.ok) {
    if (response.status === 401 && path !== 'login') showLogin();
    throw new Error((await response.text()).trim() || 'Request failed');
  }
  return response.json();
}

function showLogin() {
  state.source?.close(); state.source = null;
  $('login').hidden = false; $('app').hidden = true;
}

async function connect() {
  await api('info');
  $('login').hidden = true; $('app').hidden = false;
  state.source?.close();
  const source = state.source = new EventSource('/api/events');
  source.addEventListener('open', () => { $('connection').textContent = 'Connected'; scheduleRefresh(); });
  source.addEventListener('change', scheduleRefresh);
  source.addEventListener('error', () => { $('connection').textContent = 'Reconnecting…'; scheduleRefresh(); });
  await loadProjects();
}

function scheduleRefresh() {
  if (state.timer) return;
  state.timer = setTimeout(() => { state.timer = null; refresh().catch(error => notice(error.message)); }, 160);
}

function acceptNavigation(data) {
  state.projects = data.projects;
  state.pins = data.pinned;
  state.warnings = data.warnings || [];
  if (!state.projects.some(p => p.id === state.folder)) state.folder = '';
  renderSessions();
}

function allSessions() {
  return state.projects.flatMap(project => project.sessions.map(session => ({ ...session, project: project.id, projectName: project.name, projectPath: project.path })))
    .sort((a, b) => (Date.parse(b.updated) - Date.parse(a.updated)) || a.project.localeCompare(b.project) || a.id.localeCompare(b.id));
}

async function loadProjects(selectID) {
  acceptNavigation(await api('navigation'));
  if (selectID) {
    openFolder(selectID);
    const session = state.projects.find(p => p.id === selectID)?.sessions[0];
    if (session) await selectSession(session.id, selectID);
    return;
  }
  const project = saved('project');
  const selected = allSessions().find(s => s.project === project && s.id === saved('session.' + project)) || allSessions()[0];
  if (selected) await selectSession(selected.id, selected.project);
  else renderSelection();
}

function openFolder(id) {
  state.tab = 'projects'; state.folder = id;
  save('tab', state.tab); save('folder', id); renderSessions();
}

function changeTab(tab) {
  state.tab = tab; state.folder = '';
  save('tab', tab); save('folder', ''); renderSessions();
}

function isPinned(project, session) {
  return state.pins.some(pin => pin.project === project && pin.session === session);
}

function renderSessions() {
  const folder = state.tab === 'projects' && state.projects.find(p => p.id === state.folder);
  for (const tab of ['projects', 'recent']) {
    $('tab-' + tab).setAttribute('aria-selected', String(state.tab === tab));
    $('tab-' + tab).tabIndex = state.tab === tab ? 0 : -1;
  }
  $('navigation-panel').setAttribute('aria-labelledby', 'tab-' + state.tab);
  $('pinned-section').hidden = !!folder;
  $('folder-header').hidden = !folder;
  $('folder-title').textContent = folder?.name || '';
  $('workspace-path').textContent = folder?.path || '';
  $('new-session').disabled = state.creating || !folder || !!folder.error;
  $('list-label').textContent = folder ? 'РАЗМОВЫ' : state.tab === 'projects' ? 'ПРАЕКТЫ' : 'АПОШНІЯ РАЗМОВЫ';
  const warnings = [...state.warnings, ...(folder?.error ? [folder.error] : [])];
  $('navigation-warning').textContent = warnings.join('\n');
  $('navigation-warning').hidden = !warnings.length;
  const sessions = allSessions();
  const pinned = state.pins.map(pin => sessions.find(s => s.project === pin.project && s.id === pin.session) || {
    id: pin.session, project: pin.project, title: 'Unavailable conversation', state: 'error', updated: '',
    projectName: state.projects.find(p => p.id === pin.project)?.name || 'Unavailable project', unavailable: true,
  });
  const main = folder ? sessions.filter(s => s.project === folder.id)
    : state.tab === 'projects' ? state.projects
    : sessions.filter(s => !isPinned(s.project, s.id));
  const used = new Set();
  renderNavigationList($('pinned'), folder ? [] : pinned, false, true, used);
  renderNavigationList($('sessions'), main, !folder && state.tab === 'projects', !folder, used);
  for (const [key, row] of state.navigationRows) {
    if (!used.has(key)) { row.element.remove(); state.navigationRows.delete(key); }
  }
  $('pinned-empty').hidden = pinned.length > 0;
  $('list-empty').hidden = main.length > 0;
  $('list-empty').textContent = folder ? 'У гэтым праекце пакуль няма размоў.' : state.tab === 'projects' ? 'Дадайце папку праекта, каб пачаць.' : 'Іншых размоў пакуль няма.';
  renderSelection();
}

function renderNavigationList(nav, entries, folders, showProject, used) {
  for (const [index, item] of entries.entries()) {
    const key = folders ? 'folder/' + item.id : 'session/' + item.project + '/' + item.id;
    used.add(key);
    let row = state.navigationRows.get(key);
    if (!row) {
      const element = document.createElement('div'); element.className = 'navigation-row';
      const button = document.createElement('button'); button.className = 'navigation-link';
      const icon = document.createElement('span');
      const text = document.createElement('span'); text.className = 'session-text';
      const title = document.createElement('span'); title.className = 'row-title';
      const detail = document.createElement('small');
      text.append(title, detail); button.append(icon, text); element.append(button);
      button.addEventListener('click', () => {
        if (folders) openFolder(item.id);
        else selectSession(item.id, item.project).catch(error => notice(error.message));
      });
      let pin;
      if (!folders) {
        pin = document.createElement('button'); pin.className = 'pin-button';
        pin.addEventListener('click', async () => {
          pin.disabled = true;
          try {
            await api('projects/' + item.project + '/sessions/' + item.id + '/pin', { pinned: !isPinned(item.project, item.id) });
            await refresh();
          } catch (error) { notice(error.message); }
          finally { pin.disabled = false; }
        });
        element.append(pin);
      }
      row = { element, button, icon, title, detail, pin }; state.navigationRows.set(key, row);
    }
    const { element, button, icon, title, detail, pin } = row;
    const active = !folders && item.project === state.project && item.id === state.session;
    element.classList.toggle('active', active);
    if (active) button.setAttribute('aria-current', 'page'); else button.removeAttribute('aria-current');
    button.disabled = !!item.unavailable;
    button.title = folders ? item.path : item.title;
    icon.className = folders ? 'folder-icon' : 'dot ' + item.state;
    icon.textContent = '';
    icon.setAttribute('aria-hidden', 'true');
    title.textContent = folders ? item.name : item.title;
    const date = item.updated && !item.updated.startsWith('0001-') ? new Date(item.updated).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '';
    detail.textContent = folders ? (item.error ? 'Недаступны · ' : item.sessions.length + ' размоў · ') + (date || item.path)
      : [showProject && item.projectName, date].filter(Boolean).join(' · ');
    detail.title = folders ? item.path : item.projectPath || '';
    if (pin) {
      const pinned = isPinned(item.project, item.id);
      pin.textContent = pinned ? '★' : '☆';
      pin.setAttribute('aria-label', (pinned ? 'Адмацаваць: ' : 'Замацаваць: ') + item.title);
      pin.setAttribute('aria-pressed', String(pinned));
      pin.title = pinned ? 'Адмацаваць размову' : 'Замацаваць размову';
    }
    if (nav.children[index] !== element) nav.insertBefore(element, nav.children[index] || null);
  }
}

function renderSelection() {
  const project = state.projects.find(p => p.id === state.project);
  const session = project?.sessions.find(s => s.id === state.session);
  $('title').textContent = session?.title || project?.name || 'Your local workspace';
  $('message').disabled = !state.session;
  $('send').disabled = !state.session || state.sending;
  $('release').hidden = !state.session;
  if (!state.session) $('status').textContent = project ? 'Start a new conversation' : 'Choose a workspace to begin';
}

function resetConversation() {
  state.generation++; state.after = 0; state.compactTurns.clear(); state.tools.clear(); state.rendered.clear();
  state.status = null; state.pending = null;
  $('message').value = ''; $('transcript').replaceChildren();
  const empty = document.createElement('div'); empty.className = 'empty';
  const heading = document.createElement('h2'); heading.textContent = state.session ? 'What’s on your mind?' : 'Make room for your next idea.';
  const hint = document.createElement('p'); hint.textContent = state.session ? 'Send a message to start working in this folder.' : 'Create a conversation or choose one from the sidebar.';
  empty.append(heading, hint); $('transcript').append(empty);
  $('approval').hidden = true; $('resume').hidden = true; $('stop').hidden = true;
  $('draft-state').textContent = 'Runs on your machine'; notice(); renderSelection();
}

async function selectSession(id, project = state.project) {
  state.project = project; save('project', project);
  state.session = id; save('session.' + state.project, id); resetConversation();
  $('message').value = saved(draftKey());
  try { state.pending = JSON.parse(saved(pendingKey(), 'null')); } catch { state.pending = null; }
  if (state.pending) $('message').value = state.pending.text;
  updateComposer(); renderSessions(); sidebar(false);
  await refresh();
  $('transcript').scrollTop = $('transcript').scrollHeight;
}

async function refresh() {
  if (state.refreshing) { state.refreshAgain = true; return; }
  if ($('app').hidden) return;
  state.refreshing = true;
  const generation = state.generation, project = state.project, session = state.session;
  try {
    const navigation = await api('navigation');
    if (generation !== state.generation) { state.refreshAgain = true; return; }
    acceptNavigation(navigation);
    if (!session) return;
    let more = true;
    while (more) {
      const page = await api('projects/' + project + '/sessions/' + session + '?after=' + state.after);
      if (generation !== state.generation) { state.refreshAgain = true; return; }
      const transcript = $('transcript');
      const follow = transcript.scrollHeight - transcript.scrollTop - transcript.clientHeight < 140;
      for (const item of page.items) renderItem(item);
      state.after = page.after; more = page.more; state.status = page.status;
      renderStatus(page.status);
      if (follow) transcript.scrollTop = transcript.scrollHeight;
    }
  } finally {
    state.refreshing = false;
    if (state.refreshAgain) { state.refreshAgain = false; scheduleRefresh(); }
  }
}

function renderItem(item) {
  if (item.kind === 'compaction') { state.compactTurns.add(item.turn); return; }
  if (state.compactTurns.has(item.turn)) return;
  if (item.kind === 'tools') {
    for (const operation of item.operations || []) renderTool(operation);
    if (item.text) renderMessage('tool-error-' + item.id, 'notice', item.text);
    return;
  }
  if (item.kind === 'user' && state.pending?.id === item.id) {
    state.pending = null; save(pendingKey(), null); save(draftKey(), null); $('message').value = ''; updateComposer();
  }
  renderMessage(item.kind + '-' + item.id, item.kind, item.text || '', item.html);
}

function renderMessage(id, role, text, html) {
  if (state.rendered.has(id)) return; state.rendered.add(id);
  $('transcript').querySelector('.empty')?.remove();
  const article = document.createElement('article'); article.className = 'message ' + role;
  const label = document.createElement('div'); label.className = 'role'; label.textContent = role === 'user' ? 'You' : role === 'assistant' ? 'Unreal' : 'Tool';
  const body = document.createElement('div'); body.className = 'body';
  if (html && role === 'assistant') {
    // HTML is generated by the server's safe Goldmark renderer (raw HTML off).
    body.innerHTML = html;
    for (const link of body.querySelectorAll('a')) { link.rel = 'noopener noreferrer'; link.target = '_blank'; }
    for (const block of body.querySelectorAll('pre')) {
      const code = block.textContent;
      const button = document.createElement('button'); button.className = 'copy-code'; button.textContent = 'Copy';
      button.addEventListener('click', async () => {
        try { await navigator.clipboard.writeText(code); button.textContent = 'Copied'; }
        catch { button.textContent = 'Select text to copy'; }
      });
      block.before(button);
    }
  } else body.textContent = text;
  article.append(label, body); $('transcript').append(article);
}

function renderTool(operation) {
  let element = state.tools.get(operation.id);
  if (!element) {
    $('transcript').querySelector('.empty')?.remove();
    element = document.createElement('details'); element.className = 'tool';
    state.tools.set(operation.id, element); $('transcript').append(element);
  }
  const serialized = JSON.stringify(operation);
  if (element.dataset.value === serialized) return; element.dataset.value = serialized;
  const summary = document.createElement('summary');
  const status = document.createElement('span'); status.className = 'tool-state'; status.textContent = operation.state;
  summary.append(status, document.createTextNode(operation.description)); element.replaceChildren(summary);
  if (operation.output) { const output = document.createElement('pre'); output.textContent = operation.output; element.append(output); }
  if (operation.diff) {
    const diff = document.createElement('pre');
    for (const line of operation.diff.split('\n')) {
      const span = document.createElement('span'); span.textContent = line + '\n';
      if (line.startsWith('+') && !line.startsWith('+++')) span.className = 'diff-add';
      if (line.startsWith('-') && !line.startsWith('---')) span.className = 'diff-remove';
      diff.append(span);
    }
    element.append(diff);
  }
  if (operation.approval_id) {
    element.open = true;
    const explanation = document.createElement('p'); explanation.textContent = 'Permission required for ssh / scp / rsync. Nothing in this command has executed. Allow the entire command once?';
    const command = document.createElement('pre'); command.textContent = 'Directory: ' + operation.directory + '\n\n' + operation.description;
    const actions = document.createElement('div'); actions.className = 'actions';
    // Capture the originating session: a stale DOM control cannot approve in a new chat.
    const target = 'projects/' + state.project + '/sessions/' + state.session + '/';
    for (const [action, label] of [['deny', 'Deny'], ['permit', 'Allow once']]) {
      const button = document.createElement('button'); button.className = action === 'permit' ? 'primary' : 'quiet'; button.textContent = label;
      button.addEventListener('click', async () => {
        for (const control of actions.children) control.disabled = true;
        try { await api(target + action, { id: operation.id, request_id: operation.approval_id }); scheduleRefresh(); }
        catch (error) { notice(error.message); for (const control of actions.children) control.disabled = false; }
      });
      actions.append(button);
    }
    element.append(explanation, command, actions);
  }
  if (operation.state === 'running' || operation.state === 'canceling') {
    const cancel = document.createElement('button'); cancel.className = 'quiet'; cancel.textContent = 'Cancel operation';
    cancel.addEventListener('click', () => act('cancel', { id: operation.id }).catch(error => notice(error.message))); element.append(cancel);
  }
}

function renderStatus(status) {
  const context = status.context;
  $('status').textContent = ({ saved: 'Saved · resume or send a message to continue', stopped: 'Stopped · history retained', working: 'Working on your machine', idle: 'Ready', waiting: 'Waiting for your decision', error: 'Work stopped' })[status.state] || status.state;
  if (context?.EstimatedTokens) $('status').textContent += ' · ~' + context.EstimatedTokens.toLocaleString() + ' context tokens';
  $('stop').hidden = !['working', 'waiting'].includes(status.state);
  $('resume').hidden = !['saved', 'stopped', 'error'].includes(status.state);
  $('approval').hidden = !context?.ApprovalID;
  for (const op of status.operations || []) renderTool(op);
  if (status.error || status.warning) notice(status.error || status.warning);
}

function updateComposer() {
  $('send').textContent = state.sending ? 'Sending…' : state.pending ? 'Retry ↑' : 'Send ↑';
  $('send').disabled = state.sending || !state.session;
  $('message').readOnly = !!state.pending || state.sending;
  $('draft-state').textContent = state.pending ? 'Awaiting confirmation · retry is safe' : 'Runs on your machine';
}

async function act(action, body = {}) {
  const generation = state.generation;
  const result = await api('projects/' + state.project + '/sessions/' + state.session + '/' + action, body);
  if (generation === state.generation) { notice(); scheduleRefresh(); }
  return result;
}

$('composer').addEventListener('submit', async event => {
  event.preventDefault(); if (state.sending || !state.session) return;
  const text = state.pending?.text ?? $('message').value;
  if (!text.trim()) return;
  if (new TextEncoder().encode(text).length > (1 << 20)) { notice('Messages can contain at most 1 MiB of UTF-8 text.'); return; }
  const generation = state.generation;
  const key = pendingKey(), draft = draftKey();
  const pending = state.pending || { id: newID(), text };
  state.pending = pending; save(key, JSON.stringify(pending)); state.sending = true; updateComposer();
  try {
    await act('send', pending);
    save(key, null); save(draft, null);
    if (generation === state.generation) { state.pending = null; $('message').value = ''; }
  } catch (error) { if (generation === state.generation) notice(error.message); }
  finally { state.sending = false; updateComposer(); }
});

$('message').addEventListener('input', () => save(draftKey(), $('message').value));
$('message').addEventListener('keydown', event => { if (event.key === 'Enter' && (event.metaKey || event.ctrlKey) && !event.isComposing) { event.preventDefault(); $('composer').requestSubmit(); } });
$('login-form').addEventListener('submit', async event => {
  event.preventDefault(); $('login-error').textContent = '';
  try { await api('login', { token: $('token').value }); $('token').value = ''; await connect(); }
  catch (error) { $('login-error').textContent = error.message; }
});
$('logout').addEventListener('click', async () => { try { await api('logout', {}); showLogin(); } catch (error) { notice(error.message); } });
for (const tab of ['projects', 'recent']) {
  $('tab-' + tab).addEventListener('click', () => changeTab(tab));
  $('tab-' + tab).addEventListener('keydown', event => {
    if (['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) {
      event.preventDefault();
      const next = event.key === 'Home' ? 'projects' : event.key === 'End' ? 'recent' : tab === 'projects' ? 'recent' : 'projects';
      changeTab(next); $('tab-' + next).focus();
    }
  });
}
$('back-projects').addEventListener('click', () => changeTab('projects'));
$('open-sidebar').addEventListener('click', () => sidebar(true));
$('close-sidebar').addEventListener('click', () => sidebar(false));
for (const id of ['add-project', 'empty-add']) $(id).addEventListener('click', () => { $('project-error').textContent = ''; $('project-dialog').showModal(); $('project-path').focus(); });
$('cancel-project').addEventListener('click', () => $('project-dialog').close());
$('project-form').addEventListener('submit', async event => {
  event.preventDefault();
  try { const project = await api('projects', { path: $('project-path').value.trim() }); $('project-dialog').close(); await loadProjects(project.id); }
  catch (error) { $('project-error').textContent = error.message; }
});
$('new-session').addEventListener('click', async () => {
  const project = state.folder; if (!project || state.creating) return;
  state.creating = true; $('new-session').disabled = true;
  try {
    const session = await api('projects/' + project + '/sessions', { id: newID() });
    if (project !== state.folder) return;
    acceptNavigation(await api('navigation'));
    if (project !== state.folder) return;
    await selectSession(session.id, project); $('message').focus();
  } catch (error) { notice(error.message); } finally { state.creating = false; renderSessions(); }
});
for (const action of ['stop', 'resume', 'release']) $(action).addEventListener('click', () => act(action).catch(error => notice(error.message)));
$('approve').addEventListener('click', () => act('compact', { request_id: state.status?.context?.ApprovalID }).catch(error => notice(error.message)));
$('decline').addEventListener('click', () => act('stop').catch(error => notice(error.message)));
document.addEventListener('visibilitychange', () => { if (!document.hidden) scheduleRefresh(); });
window.addEventListener('online', scheduleRefresh);
connect().catch(error => { if ($('app').hidden) { showLogin(); if (error.message !== 'sign in required') $('login-error').textContent = error.message; } else notice(error.message); });

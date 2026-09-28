'use strict';

const $ = id => document.getElementById(id);
const state = {
  project: '', session: '', projects: [], after: 0,
  generation: 0, compactTurns: new Set(), tools: new Map(), rendered: new Set(),
  source: null, refreshing: false, refreshAgain: false, sending: false, creating: false,
  status: null, pending: null, timer: null, action: null,
  navigationRows: new Map(), pins: [], warnings: [],
  tab: saved('tab', 'projects'), folder: saved('folder'),
  deleteTarget: null, deleting: false,
  renameTarget: null, renaming: false,
};
const plural = new Intl.PluralRules('be');
const operationLabels = { running: 'Выконваецца', canceling: 'Спыняецца', completed: 'Завершана', failed: 'Памылка', canceled: 'Скасавана', 'awaiting permission': 'Чакае дазволу' };

function saved(key, fallback = '') {
  try { return localStorage.getItem('unreal.' + key) || fallback; } catch { return fallback; }
}
function save(key, value) {
  try { if (value === null) localStorage.removeItem('unreal.' + key); else localStorage.setItem('unreal.' + key, value); }
  catch { notice('Сховішча браўзера недаступнае. Не закрывайце ўкладку, пакуль адпраўка не пацверджаная.'); }
}
function draftKey() { return 'draft.' + state.project + '.' + state.session; }
function pendingKey() { return 'pending.' + state.project + '.' + state.session; }
function notice(text = '') { $('notice').textContent = text; $('notice').hidden = !text; }
function conversationCount(count) { return count + ' ' + ({ one: 'размова', few: 'размовы', many: 'размоў', other: 'размовы' })[plural.select(count)]; }
function connection(kind) {
  $('connection').dataset.state = kind;
  $('connection').textContent = ({ connecting: 'Злучаемся…', connected: 'Злучана', reconnecting: 'Аднаўляем сувязь…', offline: 'Няма сувязі' })[kind];
}
function sidebar(open) {
  const wasOpen = $('app').classList.contains('sidebar-open');
  const modal = open && matchMedia('(max-width:760px)').matches;
  $('app').classList.toggle('sidebar-open', modal);
  $('open-sidebar').setAttribute('aria-expanded', String(modal));
  $('main').inert = modal;
  if (modal) {
    $('sidebar').setAttribute('role', 'dialog'); $('sidebar').setAttribute('aria-modal', 'true');
    $('close-sidebar').focus();
  } else {
    $('sidebar').removeAttribute('role'); $('sidebar').removeAttribute('aria-modal');
    if (wasOpen) $('open-sidebar').focus();
  }
}
function actionMenu(open, focus = false) {
  $('conversation-actions-panel').hidden = !open;
  $('toggle-actions').setAttribute('aria-expanded', String(open));
  if (focus) $(open ? 'rename-session' : 'toggle-actions').focus();
}

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

// Keep provider/tool diagnostics intact, but localize the surrounding UI and
// common application errors. Do not translate commands, paths or user content.
function errorText(text) {
  const known = {
    'sign in required': 'Каб працягнуць, увайдзіце зноў.',
    'invalid token': 'Няправільны токен доступу.',
    'use an absolute path on the server machine': 'Увядзіце поўны шлях да папкі на серверы.',
    'workspace must be a directory': 'Шлях павінен весці да існуючай папкі.',
    'unknown workspace': 'Праект не знойдзены. Абнавіце спіс праектаў.',
    'server is shutting down': 'Сервер спыняецца. Пачакайце аднаўлення сувязі.',
    'session is closed': 'Размова ўжо спыненая. Паўтарыце дзеянне.',
    'session is not running': 'Праца ў гэтай размове не запушчаная.',
    'compaction request is no longer pending': 'Запыт на сцісканне кантэксту ўжо неактуальны.',
    'message ID already belongs to different text': 'Гэтае паведамленне ўжо дасланае з іншым тэкстам.',
    'session title is required': 'Увядзіце назву размовы або пакіньце поле пустым для аўтаматычнай назвы.',
    'session title must be at most 200 characters without control characters': 'Назва можа змяшчаць да 200 сімвалаў без кіравальных знакаў.',
    'conversation no longer exists': 'Размова ўжо выдаленая. Выберыце іншую.',
  };
  return known[text] || ('Не ўдалося выканаць дзеянне.' + (text ? '\nТэхнічныя звесткі: ' + text : ''));
}
async function api(path, body, method = 'POST') {
  const options = body === undefined ? {} : { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) };
  let response;
  try { response = await fetch('/api/' + path, { ...options, cache: 'no-store' }); }
  catch { throw new Error('Няма сувязі з серверам. Праверце злучэнне і паўтарыце спробу.'); }
  if (!response.ok) {
    if (response.status === 401 && path !== 'login') showLogin();
    const text = (await response.text()).trim();
    const error = new Error(response.status === 401 ? (path === 'login' ? 'Няправільны токен доступу.' : 'Каб працягнуць, увайдзіце зноў.') : errorText(text));
    error.status = response.status;
    throw error;
  }
  return response.json();
}

function showLogin() {
  state.source?.close(); state.source = null;
  sidebar(false); actionMenu(false);
  $('login').hidden = false; $('app').hidden = true;
  $('token').focus();
}
async function connect() {
  await api('info');
  $('login').hidden = true; $('app').hidden = false;
  state.source?.close(); connection('connecting');
  const source = state.source = new EventSource('/api/events');
  source.addEventListener('open', () => { connection('connected'); scheduleRefresh(); });
  source.addEventListener('change', scheduleRefresh);
  source.addEventListener('error', () => { connection(navigator.onLine ? 'reconnecting' : 'offline'); scheduleRefresh(); });
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
  // A deletion on another device also clears this browser's saved drafts on
  // reconnect. Never infer deletion from an unavailable project's empty list.
  for (const project of state.projects) {
    if (project.error) continue;
    const ids = new Set(project.sessions.map(s => s.id));
    try {
      for (const key of Object.keys(localStorage)) {
        for (const kind of ['draft.', 'pending.']) {
          const prefix = 'unreal.' + kind + project.id + '.';
          if (key.startsWith(prefix) && !ids.has(key.slice(prefix.length))) localStorage.removeItem(key);
        }
      }
    } catch { /* A browser that blocks storage has no accessible saved drafts. */ }
    const selected = saved('session.' + project.id);
    if (selected && !ids.has(selected)) save('session.' + project.id, null);
    if (state.project === project.id && state.session && !ids.has(state.session)) {
      state.session = ''; resetConversation();
      notice('Размова выдаленая. Выберыце іншую або стварыце новую.');
    }
  }
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
  $('new-session').textContent = state.creating ? 'Ствараем…' : '＋ Новая размова';
  $('list-label').textContent = folder ? 'Размовы' : state.tab === 'projects' ? 'Праекты' : 'Апошнія размовы';
  const warnings = [...state.warnings, ...(folder?.error ? [folder.error] : [])];
  $('navigation-warning').textContent = warnings.length ? 'Не ўсе праекты даступныя.\n' + warnings.join('\n') : '';
  $('navigation-warning').hidden = !warnings.length;
  const sessions = allSessions();
  const pinned = state.pins.map(pin => sessions.find(s => s.project === pin.project && s.id === pin.session) || {
    id: pin.session, project: pin.project, title: 'Недаступная размова', state: 'error', updated: '',
    projectName: state.projects.find(p => p.id === pin.project)?.name || 'Недаступны праект', unavailable: true,
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
      const button = document.createElement('button'); button.className = 'quiet navigation-link';
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
        pin = document.createElement('button'); pin.className = 'icon pin-button';
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
    const date = item.updated && !item.updated.startsWith('0001-') ? new Date(item.updated).toLocaleString('be', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '';
    detail.textContent = folders ? (item.error ? 'Недаступны · ' : conversationCount(item.sessions.length) + ' · ') + (date || item.path)
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
  $('title').textContent = session?.title || project?.name || 'Вашы праекты';
  $('message').disabled = !state.session;
  $('conversation-actions').hidden = !state.session;
  if (!state.session) $('status').textContent = project ? 'Пачніце новую размову' : 'Выберыце праект, каб пачаць';
  updateComposer();
}
function resetConversation() {
  state.generation++; state.after = 0; state.compactTurns.clear(); state.tools.clear(); state.rendered.clear();
  state.status = null; state.pending = null; state.action = null; actionMenu(false);
  $('message').value = ''; $('transcript').replaceChildren();
  const empty = document.createElement('div'); empty.className = 'empty';
  const heading = document.createElement('h2'); heading.textContent = state.session ? 'Над чым папрацуем?' : 'Месца для новай ідэі.';
  const hint = document.createElement('p'); hint.textContent = state.session ? 'Дашліце паведамленне, каб пачаць працу ў гэтым праекце.' : 'Стварыце размову або выберыце яе ў бакавой панэлі.';
  empty.append(heading, hint); $('transcript').append(empty);
  $('approval').hidden = true; $('resume').hidden = true; $('stop').hidden = true;
  notice(); renderSelection(); updateSessionActions();
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
    if (generation !== state.generation) return;
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
  const label = document.createElement('div'); label.className = 'role'; label.textContent = role === 'user' ? 'Вы' : role === 'assistant' ? 'Unreal' : 'Інструмент';
  const body = document.createElement('div'); body.className = 'body';
  if (html && role === 'assistant') {
    // HTML is generated by the server's safe Goldmark renderer (raw HTML off).
    body.innerHTML = html;
    for (const link of body.querySelectorAll('a')) { link.rel = 'noopener noreferrer'; link.target = '_blank'; }
    for (const block of body.querySelectorAll('pre')) {
      const code = block.textContent;
      const button = document.createElement('button'); button.className = 'quiet compact copy-code'; button.textContent = 'Капіяваць';
      button.addEventListener('click', async () => {
        try { await navigator.clipboard.writeText(code); button.textContent = 'Скапіявана'; }
        catch { button.textContent = 'Вылучыце тэкст для капіявання'; }
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
  const status = document.createElement('span'); status.className = 'tool-state';
  const exit = /^failed \(exit (-?\d+)\)$/.exec(operation.state);
  status.textContent = exit ? 'Памылка (код ' + exit[1] + ')' : operationLabels[operation.state] || operation.state;
  status.dataset.state = exit ? 'failed' : operation.state;
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
  // Capture the originating session: stale controls cannot act in a new chat.
  const target = 'projects/' + state.project + '/sessions/' + state.session + '/';
  const generation = state.generation;
  if (operation.approval_id) {
    element.open = true;
    const explanation = document.createElement('p'); explanation.textContent = 'Патрэбны дазвол на ssh / scp / rsync. Ніводная частка каманды яшчэ не выкананая. Дазволіць усю каманду адзін раз?';
    const command = document.createElement('pre'); command.textContent = 'Папка: ' + operation.directory + '\n\n' + operation.description;
    const actions = document.createElement('div'); actions.className = 'actions';
    for (const [action, label] of [['deny', 'Адхіліць'], ['permit', 'Дазволіць адзін раз']]) {
      const button = document.createElement('button'); button.className = action === 'permit' ? 'primary' : 'quiet'; button.textContent = label;
      button.addEventListener('click', async () => {
        for (const control of actions.children) control.disabled = true;
        button.textContent = 'Чакаем…';
        try { await api(target + action, { id: operation.id, request_id: operation.approval_id }); scheduleRefresh(); }
        catch (error) { if (generation === state.generation) notice(error.message); button.textContent = label; for (const control of actions.children) control.disabled = false; }
      });
      actions.append(button);
    }
    element.append(explanation, command, actions);
  }
  if (operation.state === 'running' || operation.state === 'canceling') {
    const cancel = document.createElement('button'); cancel.className = 'quiet'; cancel.textContent = operation.state === 'canceling' ? 'Спыняем…' : 'Спыніць аперацыю';
    cancel.disabled = operation.state === 'canceling';
    cancel.addEventListener('click', async () => {
      cancel.disabled = true; cancel.textContent = 'Спыняем…';
      try { await api(target + 'cancel', { id: operation.id }); scheduleRefresh(); }
      catch (error) { if (generation === state.generation) notice(error.message); cancel.disabled = false; cancel.textContent = 'Спыніць аперацыю'; }
    });
    const actions = document.createElement('div'); actions.className = 'actions';
    actions.append(cancel); element.append(actions);
  }
}

function renderStatus(status) {
  const context = status.context;
  $('status').textContent = ({ saved: 'Гісторыя захаваная', stopped: 'Праца спыненая · гісторыя захаваная', working: 'Працуе на вашым камп’ютары', idle: 'Гатовы да новага паведамлення', waiting: 'Чакае вашага рашэння', error: 'Праца спыненая праз памылку' })[status.state] || 'Абнаўляем стан…';
  if (context?.EstimatedTokens) $('status').textContent += ' · ~' + context.EstimatedTokens.toLocaleString('be') + ' токенаў у кантэксце';
  $('stop').hidden = !['working', 'waiting'].includes(status.state);
  $('resume').hidden = !['saved', 'stopped', 'error'].includes(status.state);
  $('approval').hidden = !context?.ApprovalID;
  updateSessionActions();
  for (const op of status.operations || []) renderTool(op);
  if (status.error || status.warning) notice(errorText(status.error || status.warning));
}
function updateComposer() {
  $('send').textContent = state.sending ? 'Дасылаем…' : state.pending ? 'Паўтарыць ↑' : 'Даслаць ↑';
  $('send').disabled = state.sending || !state.session || !(state.pending?.text ?? $('message').value).trim();
  $('message').readOnly = !!state.pending || state.sending;
  $('draft-state').textContent = state.pending ? 'Чакаем пацвярджэння · паўтор бяспечны' : 'Працуе на вашым камп’ютары';
}
function updateSessionActions() {
  const labels = { stop: ['Спыніць', 'Спыняем…'], resume: ['Працягнуць', 'Працягваем…'], compact: ['Сціснуць кантэкст', 'Сціскаем…'] };
  for (const [id, action] of [['stop', 'stop'], ['resume', 'resume'], ['approve', 'compact'], ['decline', 'stop']]) {
    $(id).disabled = !!state.action;
    $(id).textContent = labels[action][state.action === action ? 1 : 0];
  }
}
async function sessionAction(action, body = {}) {
  if (!state.session || state.action) return;
  const generation = state.generation;
  state.action = action; updateSessionActions();
  try { await act(action, body); }
  catch (error) { if (generation === state.generation) notice(error.message); }
  finally { if (generation === state.generation) { state.action = null; updateSessionActions(); } }
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
  if (new TextEncoder().encode(text).length > (1 << 20)) { notice('Паведамленне можа займаць не больш за 1 МіБ у UTF-8.'); return; }
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
$('message').addEventListener('input', () => { save(draftKey(), $('message').value); updateComposer(); });
$('message').addEventListener('keydown', event => { if (event.key === 'Enter' && (event.metaKey || event.ctrlKey) && !event.isComposing) { event.preventDefault(); $('composer').requestSubmit(); } });
$('login-form').addEventListener('submit', async event => {
  event.preventDefault(); $('login-error').textContent = '';
  const button = event.currentTarget.querySelector('button');
  if (button.disabled) return;
  button.disabled = true; button.textContent = 'Уваходзім…';
  try { await api('login', { token: $('token').value }); $('token').value = ''; await connect(); }
  catch (error) { $('login-error').textContent = error.message; }
  finally { button.disabled = false; button.textContent = 'Увайсці'; }
});
$('logout').addEventListener('click', async () => {
  $('logout').disabled = true;
  try { await api('logout', {}); showLogin(); } catch (error) { notice(error.message); }
  finally { $('logout').disabled = false; }
});
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
matchMedia('(max-width:760px)').addEventListener('change', () => sidebar(false));
$('toggle-actions').addEventListener('click', () => actionMenu($('conversation-actions-panel').hidden, true));
document.addEventListener('click', event => { if (!$('conversation-actions').contains(event.target)) actionMenu(false); });
$('conversation-actions').addEventListener('focusout', event => { if (!$('conversation-actions').contains(event.relatedTarget)) actionMenu(false); });
document.addEventListener('keydown', event => {
  if (document.querySelector('dialog[open]')) return;
  if (event.key === 'Escape') {
    if (!$('conversation-actions-panel').hidden) { actionMenu(false, true); event.preventDefault(); }
    if ($('app').classList.contains('sidebar-open')) { sidebar(false); event.preventDefault(); }
  }
  if (event.key === 'Tab' && $('app').classList.contains('sidebar-open')) {
    const controls = [...$('sidebar').querySelectorAll('button:not(:disabled), [tabindex="0"]')].filter(e => e.getClientRects().length && e.tabIndex >= 0);
    const first = controls[0], last = controls.at(-1);
    if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last?.focus(); }
    else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus(); }
  }
});
for (const id of ['add-project', 'empty-add']) $(id).addEventListener('click', () => { $('project-error').textContent = ''; $('project-dialog').showModal(); $('project-path').focus(); });
$('cancel-project').addEventListener('click', () => $('project-dialog').close());
$('project-form').addEventListener('submit', async event => {
  event.preventDefault();
  const button = event.currentTarget.querySelector('.primary');
  if (button.disabled) return;
  button.disabled = true; button.textContent = 'Дадаём…'; $('project-error').textContent = '';
  try { const project = await api('projects', { path: $('project-path').value.trim() }); $('project-dialog').close(); await loadProjects(project.id); }
  catch (error) { $('project-error').textContent = error.message; }
  finally { button.disabled = false; button.textContent = 'Дадаць праект'; }
});
$('new-session').addEventListener('click', async () => {
  const project = state.folder; if (!project || state.creating) return;
  state.creating = true; renderSessions();
  try {
    const session = await api('projects/' + project + '/sessions', { id: newID() });
    if (project !== state.folder) return;
    acceptNavigation(await api('navigation'));
    if (project !== state.folder) return;
    await selectSession(session.id, project); $('message').focus();
  } catch (error) { notice(error.message); } finally { state.creating = false; renderSessions(); }
});
for (const action of ['stop', 'resume']) $(action).addEventListener('click', () => sessionAction(action));
$('rename-session').addEventListener('click', () => {
  if (!state.session || state.renaming) return;
  actionMenu(false, true);
  state.renameTarget = { project: state.project, session: state.session };
  $('rename-title').value = $('title').textContent;
  $('rename-error').textContent = '';
  $('rename-dialog').showModal(); $('rename-title').focus(); $('rename-title').select();
});
$('cancel-rename').addEventListener('click', () => { state.renameTarget = null; $('rename-dialog').close(); });
$('rename-dialog').addEventListener('cancel', event => {
  if (state.renaming) event.preventDefault(); else state.renameTarget = null;
});
$('rename-dialog').addEventListener('keydown', event => {
  // Repeated Escape can bypass a native dialog's cancel event in some browsers.
  if (event.key === 'Escape' && state.renaming) event.preventDefault();
});
$('rename-form').addEventListener('submit', async event => {
  event.preventDefault();
  if (state.renaming || !state.renameTarget) return;
  const title = $('rename-title').value;
  if ([...title.trim().replace(/\s+/gu, ' ')].length > 200) {
    $('rename-error').textContent = 'Назва можа змяшчаць да 200 сімвалаў.';
    $('rename-title').focus(); return;
  }
  // Keep the target captured when the dialog opened, even if live updates or
  // another pending action switch/delete the selected conversation meanwhile.
  const target = state.renameTarget;
  state.renaming = true;
  $('confirm-rename').disabled = true; $('cancel-rename').disabled = true; $('rename-title').readOnly = true;
  $('confirm-rename').textContent = 'Захоўваем…'; $('rename-error').textContent = '';
  try {
    await api('projects/' + target.project + '/sessions/' + target.session + '/rename', { title });
    if (state.renameTarget === target) { state.renameTarget = null; $('rename-dialog').close(); }
    if (state.project === target.project && state.session === target.session) notice('Назва размовы захаваная.');
    scheduleRefresh();
  } catch (error) {
    if (state.renameTarget === target) $('rename-error').textContent = error.message;
  } finally {
    state.renaming = false;
    $('confirm-rename').disabled = false; $('cancel-rename').disabled = false; $('rename-title').readOnly = false;
    $('confirm-rename').textContent = 'Захаваць';
  }
});
$('delete-session').addEventListener('click', () => {
  if (!state.session) return;
  actionMenu(false, true);
  state.deleteTarget = { project: state.project, session: state.session };
  $('delete-title').textContent = $('title').textContent;
  $('delete-error').textContent = '';
  $('delete-dialog').querySelector('details').open = false;
  $('delete-dialog').showModal(); $('cancel-delete').focus();
});
$('cancel-delete').addEventListener('click', () => $('delete-dialog').close());
$('delete-dialog').addEventListener('cancel', event => { if (state.deleting) event.preventDefault(); });
$('delete-form').addEventListener('submit', async event => {
  event.preventDefault();
  if (state.deleting || !state.deleteTarget) return;
  const { project, session } = state.deleteTarget;
  state.deleting = true; $('confirm-delete').disabled = true; $('cancel-delete').disabled = true;
  $('confirm-delete').textContent = 'Выдаляем…'; $('delete-error').textContent = '';
  try {
    const result = await api('projects/' + project + '/sessions/' + session, {}, 'DELETE');
    save('draft.' + project + '.' + session, null); save('pending.' + project + '.' + session, null);
    if (saved('session.' + project) === session) save('session.' + project, null);
    if (state.project === project && state.session === session) { state.session = ''; resetConversation(); }
    $('delete-dialog').close(); state.deleteTarget = null;
    notice(result.warning || 'Размова выдаленая лакальна.');
    scheduleRefresh();
  } catch (error) { $('delete-error').textContent = error.message; }
  finally { state.deleting = false; $('confirm-delete').disabled = false; $('cancel-delete').disabled = false; $('confirm-delete').textContent = 'Выдаліць лакальна'; }
});
$('approve').addEventListener('click', () => sessionAction('compact', { request_id: state.status?.context?.ApprovalID }));
$('decline').addEventListener('click', () => sessionAction('stop'));
document.addEventListener('visibilitychange', () => { if (!document.hidden) scheduleRefresh(); });
window.addEventListener('online', () => { connection('reconnecting'); scheduleRefresh(); });
window.addEventListener('offline', () => connection('offline'));
connect().catch(error => { if ($('app').hidden) { showLogin(); if (error.status !== 401) $('login-error').textContent = error.message; } else notice(error.message); });

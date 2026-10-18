// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

// The page of the example chat. It reads the chat through the JSON API under /chat/api/ and acts as the person picked
// in "You are". Only the fields named html, which the server renders and escapes, are inserted as HTML; every other
// value is inserted as text.
(() => {
  'use strict';

  const POLL_MS = 1500;
  const OFFLINE_MS = 5000;
  const TOAST_MS = 4000;
  const PER_ROW = 3;
  const STYLES = new Set(['primary', 'success', 'neutral', 'danger']);
  const VIEWER_KEY = 'example-chat.viewer';
  const LOG_KEY = 'example-chat.log';

  const $ = (id) => document.getElementById(id);
  const ui = {
    top: $('top'),
    bridge: $('bridge'),
    viewer: $('viewer'),
    logToggle: $('log-toggle'),
    offline: $('offline'),
    layout: $('layout'),
    channels: $('channels'),
    directs: $('directs'),
    features: $('features'),
    title: $('view-title'),
    empty: $('view-empty'),
    posts: $('posts'),
    direct: $('direct'),
    bubbles: $('bubbles'),
    directForm: $('direct-form'),
    directText: $('direct-text'),
    end: $('view-end'),
    log: $('log'),
    logEntries: $('log-entries'),
    toast: $('toast'),
  };
  const wide = window.matchMedia('(min-width: 720px)');

  const app = {
    info: null,
    infoSig: '',
    logSig: '',
    viewer: '',
    // data is what the view shows: its key, the chat version it was fetched at and its items (null until fetched).
    data: { key: '', version: null, items: null },
    fresh: true,
    roots: new Map(),
    bubbles: new Map(),
    open: new Set(),
    drafts: new Map(),
    timer: 0,
    busy: false,
    again: false,
    offline: false,
    toastTimer: 0,
  };

  const store = {
    get(key) {
      try {
        return window.localStorage.getItem(key);
      } catch {
        return null;
      }
    },
    set(key, value) {
      try {
        window.localStorage.setItem(key, value);
      } catch {
        // The choice is then kept until the page is reloaded.
      }
    },
  };

  function make(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = String(text);
    return node;
  }

  function timeOf(iso, seconds) {
    const node = make('time', 'when');
    const at = new Date(iso);
    if (iso && !Number.isNaN(at.getTime())) {
      node.dateTime = iso;
      node.textContent = at.toLocaleTimeString([], seconds
        ? { hour: '2-digit', minute: '2-digit', second: '2-digit' }
        : { hour: '2-digit', minute: '2-digit' });
      node.title = at.toLocaleString();
    }
    return node;
  }

  function personName(login) {
    const person = app.info && app.info.people.find((p) => p.login === login);
    return person ? person.name : login || 'Someone';
  }

  function decode(s) {
    try {
      return decodeURIComponent(s);
    } catch {
      return '';
    }
  }

  // API

  async function answer(res) {
    let body = null;
    try {
      body = await res.json();
    } catch {
      body = null;
    }
    if (!res.ok) {
      throw new Error((body && body.error) || `The bridge answered ${res.status}.`);
    }
    return body;
  }

  async function getJSON(path) {
    return answer(await fetch(path, { cache: 'no-store' }));
  }

  async function postJSON(path, body) {
    return answer(await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }));
  }

  // Views

  function currentView() {
    if (window.location.hash === '#/direct') {
      return { type: 'direct', key: `direct:${app.viewer}` };
    }
    const channels = app.info ? app.info.channels : [];
    let id = window.location.hash.startsWith('#/c/') ? decode(window.location.hash.slice(4)) : '';
    if (!channels.some((c) => c.id === id)) id = channels.length ? channels[0].id : '';
    return { type: 'channel', id, key: `channel:${id}` };
  }

  // showFrame sets the title, the navigation and the visible section of the view, and forgets what another view
  // showed.
  function showFrame(view) {
    const channel = view.type === 'channel' && app.info.channels.find((c) => c.id === view.id);
    ui.title.textContent = channel ? channel.name : 'Direct chat with the bot';
    for (const a of document.querySelectorAll('.nav-link')) {
      if (a.dataset.view === view.key || (a.dataset.view === 'direct' && view.type === 'direct')) {
        a.setAttribute('aria-current', 'page');
      } else {
        a.removeAttribute('aria-current');
      }
    }
    ui.posts.hidden = view.type !== 'channel';
    ui.direct.hidden = view.type !== 'direct';
    if (app.data.key !== view.key) {
      app.data = { key: view.key, version: null, items: null };
      app.fresh = true;
      ui.posts.replaceChildren();
      app.roots.clear();
      ui.bubbles.replaceChildren();
      app.bubbles.clear();
      ui.empty.textContent = 'Loading…';
      ui.empty.hidden = false;
    }
  }

  function render(view) {
    const items = app.data.items;
    if (!items) return;
    const pin = atEnd();
    if (view.type === 'direct') {
      reconcile(ui.bubbles, app.bubbles, items, newBubble, updateBubble);
    } else {
      reconcile(ui.posts, app.roots, items, newRoot, updateRoot);
    }
    ui.empty.textContent = view.type === 'direct'
      ? 'No messages yet. The bot writes to you here.'
      : 'No messages in this channel yet.';
    ui.empty.hidden = items.length > 0;
    if ((app.fresh && wide.matches) || (!app.fresh && pin)) toEnd();
    app.fresh = false;
  }

  // atEnd tells whether the end of the view is on screen, so that new messages keep it there.
  function atEnd() {
    const top = ui.end.getBoundingClientRect().top;
    return top >= 0 && top <= window.innerHeight + 48;
  }

  function toEnd() {
    if (ui.end.getBoundingClientRect().top > window.innerHeight) ui.end.scrollIntoView({ block: 'end' });
  }

  // reconcile makes the children of list the elements of items, in order: an element whose item is new is created,
  // one whose item is gone is removed, and the others stay in place, so that focus and drafts survive.
  function reconcile(list, records, items, create, update) {
    const ids = new Set(items.map((item) => item.id));
    for (const [id, rec] of records) {
      if (!ids.has(id)) {
        rec.li.remove();
        records.delete(id);
      }
    }
    let cursor = list.firstElementChild;
    for (const item of items) {
      let rec = records.get(item.id);
      if (!rec) {
        rec = create(item);
        records.set(item.id, rec);
      }
      update(rec, item);
      if (rec.li === cursor) {
        cursor = cursor.nextElementSibling;
      } else {
        list.insertBefore(rec.li, cursor);
      }
    }
  }

  // swap replaces old with fresh and gives the focus back to the element with the same data-key.
  function swap(old, fresh) {
    const active = document.activeElement;
    const key = active && old.contains(active) ? active.dataset.key : '';
    old.replaceWith(fresh);
    if (key) {
      const again = fresh.querySelector(`[data-key="${CSS.escape(key)}"]`);
      if (again) again.focus({ preventScroll: true });
    }
  }

  function signature(item) {
    return `${app.viewer}\n${JSON.stringify(item)}`;
  }

  // Channel posts

  function newRoot(p) {
    const li = make('li', 'post');
    li.dataset.id = p.id;
    const main = make('div', 'post-main');
    const thread = make('div', 'thread');
    const toggle = make('button', 'thread-toggle');
    toggle.type = 'button';
    toggle.dataset.action = 'thread';
    toggle.dataset.root = p.id;
    toggle.dataset.key = `thread:${p.id}`;
    const panel = make('div', 'thread-panel');
    panel.id = `thread-${p.id}`;
    toggle.setAttribute('aria-controls', panel.id);
    const list = make('ol', 'thread-posts');
    const form = make('form', 'compose thread-form');
    form.dataset.root = p.id;
    const label = make('label', 'sr-only', 'Write in the Thread');
    const text = make('textarea');
    text.id = `thread-text-${p.id}`;
    text.rows = 1;
    text.maxLength = 4000;
    text.placeholder = 'Write in the Thread';
    text.dataset.root = p.id;
    text.value = app.drafts.get(p.id) || '';
    label.htmlFor = text.id;
    const send = make('button', 'btn btn-primary', 'Send');
    send.type = 'submit';
    form.append(label, text, send);
    panel.append(list, form);
    thread.append(toggle, panel);
    li.append(main, thread);
    return { li, main, thread, toggle, panel, list, form, sig: '', items: new Map() };
  }

  function updateRoot(rec, p) {
    const posts = p.thread || [];
    const sig = signature({ ...p, thread: undefined });
    if (sig !== rec.sig) {
      rec.sig = sig;
      const fresh = postContent(p, true);
      swap(rec.main, fresh);
      rec.main = fresh;
      if (!p.deleted && p.color && CSS.supports('color', p.color)) {
        rec.li.style.setProperty('--status', p.color);
      } else {
        rec.li.style.removeProperty('--status');
      }
      rec.li.classList.toggle('deleted', p.deleted);
    }
    const open = app.open.has(p.id);
    rec.toggle.textContent = `Thread (${posts.filter((t) => !t.deleted).length})`;
    rec.toggle.setAttribute('aria-expanded', String(open));
    rec.panel.hidden = !open;
    rec.form.hidden = p.deleted;
    rec.thread.hidden = p.deleted && posts.length === 0;
    reconcile(rec.list, rec.items, posts, newThreadPost, updateThreadPost);
  }

  function newThreadPost(t) {
    const li = make('li', 'tpost');
    li.dataset.id = t.id;
    const main = make('div');
    li.append(main);
    return { li, main, sig: '' };
  }

  function updateThreadPost(rec, t) {
    const sig = signature(t);
    if (sig === rec.sig) return;
    rec.sig = sig;
    const fresh = postContent(t, false);
    swap(rec.main, fresh);
    rec.main = fresh;
    rec.li.classList.toggle('from-bot', t.bot);
    rec.li.classList.toggle('mine', !t.bot && t.author === app.viewer);
  }

  // postContent is everything a post shows except its Thread.
  function postContent(p, root) {
    const box = make('div', root ? 'post-main' : 'tpost-main');
    if (p.deleted) {
      box.append(make('p', 'tombstone', 'This message was deleted.'));
      return box;
    }
    const head = make('div', 'post-head');
    if (root) {
      const title = make('h2', 'post-title');
      if (p.emoji) {
        const emoji = make('span', 'emoji', p.emoji);
        emoji.setAttribute('aria-hidden', 'true');
        title.append(emoji, ' ');
      }
      if (p.link && p.link.url) {
        const a = make('a', null, p.title);
        a.href = p.link.url;
        a.rel = 'noopener noreferrer';
        a.target = '_blank';
        a.dataset.key = `link:${p.id}`;
        if (p.link.label) a.title = p.link.label;
        title.append(a);
      } else {
        title.append(p.title);
      }
      head.append(title);
      if (p.status) head.append(make('span', `badge status status-${p.status}`, p.status));
      if (p.kind === 'test' || p.kind === 'storm_summary') {
        head.append(make('span', 'badge kind', p.kind === 'test' ? 'test' : 'storm summary'));
      }
    } else {
      head.append(make('span', 'author', p.bot ? 'Bot' : personName(p.author)));
    }
    if (p.quiet) head.append(make('span', 'badge quiet', 'quiet'));
    if (p.edited_at) {
      const edited = make('span', 'badge edited', 'edited');
      edited.title = `Edited ${p.edits} ${p.edits === 1 ? 'time' : 'times'}`;
      head.append(edited);
    }
    head.append(timeOf(p.posted_at));
    if (p.bot || p.author === app.viewer) {
      const del = make('button', 'action', 'Delete');
      del.type = 'button';
      del.dataset.action = 'delete';
      del.dataset.id = p.id;
      del.dataset.key = `delete:${p.id}`;
      head.append(del);
    }
    box.append(head);
    box.append(bodyOf(p.html));
    if (p.buttons && p.buttons.length) box.append(buttonRows(p));
    for (const n of (p.notes || []).filter((x) => x.for === app.viewer)) {
      const note = make('div', 'note');
      note.append(make('span', 'note-label', 'Only you can see this'), make('span', 'note-text', n.text));
      box.append(note);
    }
    return box;
  }

  function bodyOf(html) {
    const body = make('div', 'body');
    body.innerHTML = html || '';
    for (const m of body.querySelectorAll('.mention')) {
      m.classList.toggle('me', m.dataset.person === app.viewer);
    }
    return body;
  }

  function buttonRows(p) {
    const rows = make('div', 'buttons');
    for (let i = 0; i < p.buttons.length; i += PER_ROW) {
      const row = make('div', 'btn-row');
      p.buttons.slice(i, i + PER_ROW).forEach((b, j) => {
        const index = i + j;
        const btn = make('button', `btn btn-${STYLES.has(b.style) ? b.style : 'neutral'}`, b.label);
        btn.type = 'button';
        btn.dataset.action = 'press';
        btn.dataset.id = p.id;
        btn.dataset.index = String(index);
        btn.dataset.key = `press:${p.id}:${index}`;
        row.append(btn);
      });
      rows.append(row);
    }
    return rows;
  }

  // Direct chat

  function newBubble(e) {
    const li = make('li', 'bubble');
    li.dataset.id = e.id;
    return { li, sig: '' };
  }

  function updateBubble(rec, e) {
    const sig = signature(e);
    if (sig === rec.sig) return;
    rec.sig = sig;
    rec.li.className = `bubble ${e.bot ? 'from-bot' : 'mine'}`;
    const head = make('div', 'bubble-head');
    head.append(make('span', 'author', e.bot ? 'Bot' : personName(e.author)), timeOf(e.at));
    rec.li.replaceChildren(head, bodyOf(e.html));
  }

  // State: header, people, sidebar and event log

  function applyState(s) {
    app.info = s;
    const sig = JSON.stringify([s.bridge, s.features, s.channels, s.people]);
    if (sig !== app.infoSig) {
      app.infoSig = sig;
      ui.bridge.textContent = `${s.bridge.name} ${s.bridge.version}`;
      ui.bridge.title = `${s.messenger}; events by ${s.push ? 'push' : 'polling'}`;
      renderPeople(s.people);
      renderSidebar(s);
    }
    const logSig = JSON.stringify(s.log);
    if (logSig !== app.logSig) {
      app.logSig = logSig;
      renderLog(s.log);
    }
  }

  function renderPeople(people) {
    const has = (login) => login && people.some((p) => p.login === login);
    const stored = store.get(VIEWER_KEY);
    const login = [app.viewer, stored].find(has) || (people.length ? people[0].login : '');
    ui.viewer.replaceChildren(...people.map((p) => {
      const option = make('option', null, `${p.name} (${p.login})`);
      option.value = p.login;
      return option;
    }));
    ui.viewer.value = login;
    app.viewer = login;
  }

  function navLink(text, hash, view) {
    const li = make('li');
    const a = make('a', 'nav-link', text);
    a.href = hash;
    a.dataset.view = view;
    li.append(a);
    return li;
  }

  function renderSidebar(s) {
    ui.channels.replaceChildren(...s.channels.map((c) => navLink(c.name, `#/c/${encodeURIComponent(c.id)}`,
      `channel:${c.id}`)));
    ui.directs.replaceChildren(navLink('Direct chat with the bot', '#/direct', 'direct'));
    ui.features.replaceChildren(...Object.entries(s.features).map(([name, on]) => {
      const li = make('li', `feature ${on ? 'on' : 'off'}`, name);
      li.title = on ? 'offered' : 'not offered';
      li.append(make('span', 'sr-only', on ? ' (on)' : ' (off)'));
      return li;
    }));
  }

  function outcomeClass(outcome) {
    if (outcome === 'reported' || /^[23]\d\d$/.test(outcome)) return 'ok';
    if (outcome === 'not_offered' || outcome === 'none') return 'muted';
    return 'bad';
  }

  function renderLog(log) {
    if (!log.length) {
      ui.logEntries.replaceChildren(make('li', 'empty', 'Nothing yet.'));
      return;
    }
    ui.logEntries.replaceChildren(...log.map((e) => {
      const li = make('li', 'log-entry');
      li.append(timeOf(e.at, true), make('span', `chip kind-${e.kind === 'report' ? 'report' : 'call'}`, e.kind),
        make('span', 'log-what', e.what), make('span', `log-outcome ${outcomeClass(e.outcome)}`, e.outcome));
      if (e.count > 1) li.append(make('span', 'log-count', `×${e.count}`));
      return li;
    }));
  }

  function setOffline(offline) {
    if (offline === app.offline) return;
    app.offline = offline;
    ui.offline.hidden = !offline;
  }

  function setLogShown(shown) {
    ui.log.hidden = !shown;
    ui.layout.classList.toggle('no-log', !shown);
    ui.logToggle.setAttribute('aria-expanded', String(shown));
    store.set(LOG_KEY, shown ? 'shown' : 'hidden');
  }

  function toast(text, error) {
    window.clearTimeout(app.toastTimer);
    ui.toast.textContent = text;
    ui.toast.classList.toggle('error', Boolean(error));
    ui.toast.classList.add('show');
    app.toastTimer = window.setTimeout(() => ui.toast.classList.remove('show'), TOAST_MS);
  }

  // Polling

  async function tick() {
    app.timer = 0;
    if (document.hidden) return;
    app.busy = true;
    let delay = POLL_MS;
    try {
      const s = await getJSON('/chat/api/state');
      applyState(s);
      const view = currentView();
      showFrame(view);
      if (app.data.items === null || app.data.version !== s.version) {
        const path = view.type === 'direct'
          ? `/chat/api/direct-messages?user=${encodeURIComponent(app.viewer)}`
          : `/chat/api/messages?channel=${encodeURIComponent(view.id)}`;
        const items = await getJSON(path);
        if (currentView().key === view.key) {
          app.data = { key: view.key, version: s.version, items };
          render(view);
        } else {
          app.again = true;
        }
      }
      setOffline(false);
    } catch {
      setOffline(true);
      delay = OFFLINE_MS;
    } finally {
      app.busy = false;
    }
    const again = app.again;
    app.again = false;
    schedule(again ? 0 : delay);
  }

  function schedule(delay) {
    window.clearTimeout(app.timer);
    app.timer = 0;
    if (!document.hidden) app.timer = window.setTimeout(tick, delay);
  }

  // kick fetches at once, after the fetch in flight if there is one.
  function kick() {
    if (app.busy) {
      app.again = true;
      return;
    }
    schedule(0);
  }

  function refresh() {
    if (!app.info) return;
    const view = currentView();
    showFrame(view);
    render(view);
    kick();
  }

  // Actions

  function refocus(key) {
    const active = document.activeElement;
    if (active && active !== document.body) return;
    const again = document.querySelector(`[data-key="${CSS.escape(key)}"]`);
    if (again) again.focus({ preventScroll: true });
  }

  async function press(btn) {
    if (btn.disabled) return;
    const key = btn.dataset.key;
    const focused = document.activeElement === btn;
    btn.disabled = true;
    btn.classList.add('pressing');
    try {
      const res = await postJSON('/chat/api/press', {
        message_id: btn.dataset.id,
        user: app.viewer,
        button: Number(btn.dataset.index),
      });
      toast(res.confirmation);
    } catch (err) {
      toast(err.message, true);
    } finally {
      btn.disabled = false;
      btn.classList.remove('pressing');
      if (focused) refocus(key);
      kick();
    }
  }

  async function remove(btn) {
    if (btn.disabled) return;
    btn.disabled = true;
    try {
      const res = await postJSON('/chat/api/delete', { message_id: btn.dataset.id, user: app.viewer });
      toast(res.reported === 'not_offered'
        ? 'Deleted. This bridge does not report deletions to Muster.'
        : 'Deleted.');
    } catch (err) {
      btn.disabled = false;
      toast(err.message, true);
    } finally {
      kick();
    }
  }

  async function sendThread(form) {
    const text = form.querySelector('textarea');
    const send = form.querySelector('button');
    const root = form.dataset.root;
    if (!text.value.trim() || send.disabled) return;
    send.disabled = true;
    try {
      const res = await postJSON('/chat/api/thread', { message_id: root, user: app.viewer, text: text.value });
      text.value = '';
      app.drafts.delete(root);
      if (res.reported === 'not_offered') toast('Posted. This bridge does not report Thread messages to Muster.');
    } catch (err) {
      toast(err.message, true);
    } finally {
      send.disabled = false;
      kick();
    }
  }

  async function sendDirect() {
    const send = ui.directForm.querySelector('button');
    if (!ui.directText.value.trim() || send.disabled) return;
    send.disabled = true;
    try {
      await postJSON('/chat/api/direct-message', { user: app.viewer, text: ui.directText.value });
      ui.directText.value = '';
    } catch (err) {
      toast(err.message, true);
    } finally {
      send.disabled = false;
      kick();
    }
  }

  function toggleThread(root) {
    const rec = app.roots.get(root);
    if (!rec) return;
    if (app.open.has(root)) {
      app.open.delete(root);
    } else {
      app.open.add(root);
    }
    const open = app.open.has(root);
    rec.toggle.setAttribute('aria-expanded', String(open));
    rec.panel.hidden = !open;
  }

  document.addEventListener('click', (ev) => {
    const target = ev.target instanceof Element ? ev.target.closest('[data-action]') : null;
    if (!target) return;
    switch (target.dataset.action) {
      case 'press':
        press(target);
        break;
      case 'delete':
        remove(target);
        break;
      case 'thread':
        toggleThread(target.dataset.root);
        break;
      case 'log':
        setLogShown(ui.log.hidden);
        break;
      default:
    }
  });

  document.addEventListener('submit', (ev) => {
    const form = ev.target;
    ev.preventDefault();
    if (form === ui.directForm) {
      sendDirect();
    } else if (form.classList.contains('thread-form')) {
      sendThread(form);
    }
  });

  document.addEventListener('input', (ev) => {
    const t = ev.target;
    if (t instanceof HTMLTextAreaElement && t.dataset.root) app.drafts.set(t.dataset.root, t.value);
  });

  // Enter sends, Shift+Enter starts a new line.
  document.addEventListener('keydown', (ev) => {
    const t = ev.target;
    if (ev.key === 'Enter' && !ev.shiftKey && !ev.isComposing && t instanceof HTMLTextAreaElement && t.form) {
      ev.preventDefault();
      t.form.requestSubmit();
    }
  });

  document.addEventListener('change', (ev) => {
    if (ev.target !== ui.viewer) return;
    app.viewer = ui.viewer.value;
    store.set(VIEWER_KEY, app.viewer);
    refresh();
  });

  window.addEventListener('hashchange', refresh);

  document.addEventListener('visibilitychange', () => {
    if (document.hidden) {
      window.clearTimeout(app.timer);
      app.timer = 0;
    } else {
      kick();
    }
  });

  new ResizeObserver(() => {
    document.documentElement.style.setProperty('--header-h', `${ui.top.offsetHeight}px`);
  }).observe(ui.top);

  setLogShown(store.get(LOG_KEY) !== 'hidden');
  tick();
})();

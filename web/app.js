'use strict';

var _session = null;
var _configDoc = null;
var _sessionMonitor = null;

function escapeHtml(str) {
  if (typeof str !== 'string') str = String(str);
  return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

function apiRequest(url, opts) {
  opts = opts || {};
  var fetchOpts = {
    method: opts.method || 'GET',
    credentials: 'same-origin',
    headers: opts.body ? { 'Content-Type': 'application/json' } : undefined,
    body: opts.body ? JSON.stringify(opts.body) : undefined
  };
  return fetch(url, fetchOpts).then(function(r) {
    return r.text().then(function(text) {
      var data = {};
      if (text) {
        try { data = JSON.parse(text); } catch (e) { data = { error: text }; }
      }
      if (!r.ok) {
        var err = new Error(data.error || r.statusText || 'Request failed');
        err.status = r.status;
        err.data = data;
        throw err;
      }
      if (opts.cache) {
        try { sessionStorage.setItem('cache:' + url, JSON.stringify(data)); } catch (e) {}
      }
      return data;
    });
  });
}

function apiGet(url, opts) { return apiRequest(url, opts); }
function apiPost(url, data) { return apiRequest(url, { method: 'POST', body: data }); }
function apiPut(url, data) { return apiRequest(url, { method: 'PUT', body: data }); }
function apiPutRawJSON(url, jsonText) {
  return fetch(url, {
    method: 'PUT',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: jsonText
  }).then(function(r) {
    return r.text().then(function(text) {
      var data = {};
      if (text) {
        try { data = JSON.parse(text); } catch (e) { data = { error: text }; }
      }
      if (!r.ok) {
        var err = new Error(data.error || r.statusText || 'Request failed');
        err.status = r.status;
        err.data = data;
        throw err;
      }
      return data;
    });
  });
}

function handleApiError(err, fallback) {
  if (err && (err.status === 401 || err.status === 403)) {
    _session = null;
    stopPolling();
    show('loginView');
    showToast(err.message || 'Authentication required', 'error');
    return;
  }
  if (fallback) showToast(fallback, 'error');
}

function getCached(url) {
  try {
    var raw = sessionStorage.getItem('cache:' + url);
    return raw ? JSON.parse(raw) : null;
  } catch (e) { return null; }
}

function showToast(msg, type) {
  var t = document.getElementById('_toast');
  if (!t) {
    t = document.createElement('div');
    t.id = '_toast';
    document.body.appendChild(t);
  }
  t.textContent = msg;
  t.className = 'toast toast-' + (type || 'info') + ' show';
  clearTimeout(t._tid);
  if (type === 'warning') return;
  t._tid = setTimeout(function() { t.classList.remove('show'); }, 3500);
}

function switchPage(pageId) {
  document.querySelectorAll('.page').forEach(function(p) { p.classList.add('hidden'); });
  document.getElementById(pageId + 'Page').classList.remove('hidden');
  document.querySelectorAll('.nav-links a, .nav-links .nav-dropdown-toggle').forEach(function(a) { a.classList.remove('active'); });
  var link = document.querySelector('.nav-links a[data-page="' + pageId + '"]');
  if (link) link.classList.add('active');
  /* highlight parent dropdown toggle if any */
  var parentLi = link ? link.closest('.nav-dropdown') : null;
  if (parentLi) {
    var toggle = parentLi.querySelector('.nav-dropdown-toggle');
    if (toggle) toggle.classList.add('active');
  }
  if (pageId === 'about') renderAbout();
  if (pageId === 'diagnostics') renderDiagnostics();
  if (pageId === 'configHa') loadConfig().then(renderHomeAssistantConfig).catch(function(err) { handleApiError(err, 'Failed to load config'); });
  if (pageId === 'configEntrypoints') loadConfig().then(renderEntrypointsConfig).catch(function(err) { handleApiError(err, 'Failed to load config'); });
  if (pageId === 'adminUser') renderAdminUser();
}

/* ──── Nav bar ──── */
function buildNav() {
  var nav = document.getElementById('nav');
  if (!nav) return;
  var pages = [
    {
      label: 'Config',
      children: [
        { id: 'configEntrypoints', label: 'Entrypoints' }
      ]
    },
    {
      label: 'Logger',
      children: [
        { id: 'logs', label: 'Logs' },
        { id: 'busframes', label: 'BUS Frames' }
      ]
    },
    { id: 'about', label: 'About' },
    { id: 'diagnostics', label: 'Diagnostics' },
    {
      label: 'Admin',
      children: [
        { id: 'adminUser', label: 'User' },
        { action: 'restartCompanion()', label: 'Restart' },
        { action: 'logout()', label: 'Logout' }
      ]
    }
  ];
  var html = '<span class="nav-brand"><img src="/bticino-logo.svg" height="24" alt=""> BTicino Companion</span><ul class="nav-links">';
  for (var i = 0; i < pages.length; i++) {
    var pg = pages[i];
    if (pg.children) {
      var childActive = false;
      for (var c = 0; c < pg.children.length; c++) {
        if (document.getElementById(pg.children[c].id + 'Page') && !document.getElementById(pg.children[c].id + 'Page').classList.contains('hidden')) {
          childActive = true;
          break;
        }
      }
      html += '<li class="nav-dropdown">';
      html += '<a class="nav-dropdown-toggle' + (childActive ? ' active' : '') + '">' + escapeHtml(pg.label) + '</a>';
      html += '<ul class="nav-dropdown-menu">';
      for (var j = 0; j < pg.children.length; j++) {
        var ch = pg.children[j];
        if (ch.action) {
          html += '<li><a onclick="' + ch.action + '">' + escapeHtml(ch.label) + '</a></li>';
        } else {
          html += '<li><a data-page="' + ch.id + '" onclick="switchPage(\'' + ch.id + '\')">' + escapeHtml(ch.label) + '</a></li>';
        }
      }
      html += '</ul></li>';
    } else {
      html += '<li><a data-page="' + pg.id + '" onclick="switchPage(\'' + pg.id + '\')">' + escapeHtml(pg.label) + '</a></li>';
    }
  }
  html += '</ul>';
  nav.innerHTML = html;
  /* activate first page */
  var first = document.querySelector('.nav-links a[data-page]');
  if (first) first.classList.add('active');
}

function toggleVis(btn) {
  var inp = btn.previousElementSibling;
  var show = inp.type === 'password';
  inp.type = show ? 'text' : 'password';
  btn.style.opacity = show ? '1' : '0.4';
}

/* ──── Session ──── */
function checkSession() {
  return apiGet('/webui/api/session', { cache: true }).then(function(session) {
    _session = session;
    if (!session.authenticated) {
      stopSessionMonitor();
      renderLoginHelp(session.bootstrap_required);
      resetLoginForm();
      show('loginView');
      return;
    }
    if (session.bootstrap) {
      stopSessionMonitor();
      show('setupView');
      return;
    }
    startSessionMonitor();
    show('appView');
    buildNav();
    switchPage('about');
  }).catch(function(err) {
    resetLoginForm();
    show('loginView');
    showToast(err.message || 'Failed to connect', 'error');
  });
}

function renderLoginHelp(bootstrapRequired) {
  var help = document.getElementById('loginHelp');
  if (!help) return;
  if (bootstrapRequired) {
    help.innerHTML = 'First login uses <strong>companion / companion</strong>, then you must replace the default credentials.';
    help.style.display = 'block';
    return;
  }
  help.style.display = 'none';
}

function show(viewId) {
  document.getElementById('loginView').classList.add('hidden');
  document.getElementById('setupView').classList.add('hidden');
  document.getElementById('appView').classList.add('hidden');
  document.getElementById(viewId).classList.remove('hidden');
}

function resetLoginForm() {
  var form = document.getElementById('loginForm');
  if (!form) return;
  form.reset();
  var button = form.querySelector('.btn');
  if (!button) return;
  button.disabled = false;
  button.textContent = 'Sign In';
}

function startSessionMonitor() {
  if (_sessionMonitor) return;
  _sessionMonitor = setInterval(function() {
    apiGet('/webui/api/session').then(function(session) {
      if (session.authenticated) return;
      _session = null;
      stopSessionMonitor();
      renderLoginHelp(session.bootstrap_required);
      resetLoginForm();
      show('loginView');
      showToast('Companion restarted. Sign in again.', 'info');
    }).catch(function() {});
  }, 5000);
}

function stopSessionMonitor() {
  if (!_sessionMonitor) return;
  clearInterval(_sessionMonitor);
  _sessionMonitor = null;
}

/* ──── Login ──── */
function submitLogin(event) {
  event.preventDefault();
  var form = new FormData(event.target);
  var btn = event.target.querySelector('.btn');
  btn.disabled = true;
  btn.textContent = 'Signing In…';
  var errEl = document.getElementById('loginError');
  errEl.classList.remove('visible');

  apiPost('/webui/api/login', {
    username: form.get('username'),
    password: form.get('password')
  }).then(function(data) {
    if (data.error) {
      errEl.textContent = data.error;
      errEl.classList.add('visible');
      btn.disabled = false;
      btn.textContent = 'Sign In';
      return;
    }
    btn.textContent = 'Sign In';
    checkSession();
  }).catch(function(err) {
    errEl.textContent = err.message || 'Connection error';
    errEl.classList.add('visible');
    btn.disabled = false;
    btn.textContent = 'Sign In';
  });
}

function submitSetup(event) {
  event.preventDefault();
  var form = new FormData(event.target);
  var btn = event.target.querySelector('.btn');
  btn.disabled = true;
  btn.textContent = 'Saving…';
  var errEl = document.getElementById('setupError');
  errEl.classList.remove('visible');

  apiPost('/webui/api/credentials', {
    username: form.get('username'),
    password: form.get('password'),
    current_password: ''
  }).then(function(data) {
    if (data.error) {
      errEl.textContent = data.error;
      errEl.classList.add('visible');
      btn.disabled = false;
      btn.textContent = 'Save Credentials';
      return;
    }
    showToast('Credentials saved. Sign in with the new account.', 'success');
    btn.textContent = 'Save Credentials';
    resetLoginForm();
    show('loginView');
  }).catch(function(err) {
    errEl.textContent = err.message || 'Connection error';
    errEl.classList.add('visible');
    btn.disabled = false;
    btn.textContent = 'Save Credentials';
  });
}

function logout() {
  apiPost('/webui/api/logout', {}).then(function() {
    showToast('Signed out.', 'info');
    _session = null;
    stopSessionMonitor();
    resetLoginForm();
    show('loginView');
  }).catch(function(err) {
    showToast(err.message || 'Logout failed', 'error');
  });
}

function restartCompanion() {
  showToast('Restarting Companion...', 'warning');
  apiPost('/webui/api/restart', {}).then(function() {
    setTimeout(pollRestartReady, 1500);
  }).catch(function(err) {
    showToast(err.message || 'Restart failed', 'error');
  });
}

function pollRestartReady() {
  var attempts = 0;
  var timer = setInterval(function() {
    attempts++;
    apiGet('/webui/api/session').then(function(session) {
      clearInterval(timer);
      _session = session;
      showToast('Companion restarted.', 'success');
      checkSession();
    }).catch(function() {
      if (attempts >= 30) {
        clearInterval(timer);
        showToast('Restart is taking longer than expected. Refresh the page soon.', 'warning');
      }
    });
  }, 1000);
}

/* ──── Config pages ──── */
function loadConfig() {
  return apiGet('/webui/api/config').then(function(data) {
    _configDoc = data;
    return _configDoc;
  });
}

function saveConfig(statusEl, saveBtn) {
  if (!_configDoc) return Promise.reject(new Error('Config is not loaded'));
  return apiPut('/webui/api/config', _configDoc).then(function() {
    if (statusEl) setStatus(statusEl.id, '');
    flashSavedButton(saveBtn);
    showToast('Saved. Restart Companion to apply changes.', 'warning');
  }).catch(function(err) {
    if (statusEl) {
      statusEl.textContent = err.message || 'Save failed';
      statusEl.style.color = 'var(--danger)';
    }
    throw err;
  });
}

function flashSavedButton(btn) {
  if (!btn) return;
  clearTimeout(btn._savedTimer);
  if (!btn._defaultText) btn._defaultText = btn.textContent;
  btn.disabled = true;
  btn.textContent = 'Saved';
  btn.classList.remove('btn-primary');
  btn.classList.add('btn-success');
  btn._savedTimer = setTimeout(function() {
    btn.disabled = false;
    btn.textContent = btn._defaultText || 'Save';
    btn.classList.remove('btn-success');
    btn.classList.add('btn-primary');
  }, 2000);
}

function ensureCompanionConfig() {
  _configDoc.companion = _configDoc.companion || {};
  return _configDoc.companion;
}

function renderHomeAssistantConfig() {
  var auth = (_configDoc.companion && _configDoc.companion.auth) || {};
  var cfg = ensureCompanionConfig();
  var form = document.getElementById('haConfigForm');
  form.device_id.value = auth.device_id || '';
  form.key_id.value = auth.key_id || '';
  form.claim_code.value = auth.claim_code || '';
  form.bearer_token.value = auth.bearer_token || '';
  renderHABadge(!!auth.claimed);
  renderIceServers((cfg.webrtc && cfg.webrtc.ice_servers) || []);
  setStatus('haConfigStatus', '');
}

function renderHABadge(claimed) {
  var badge = document.getElementById('haConfigBadge');
  if (!badge) return;
  badge.textContent = claimed ? 'Claimed' : 'Claimable';
  badge.className = 'badge ' + (claimed ? 'badge-success' : 'badge-info');
}

function saveHomeAssistantConfig(saveBtn) {
  var form = document.getElementById('haConfigForm');
  var cfg = ensureCompanionConfig();
  var iceServers = collectIceServers();
  if (!iceServers) return;
  _configDoc.companion.auth = _configDoc.companion.auth || {};
  _configDoc.companion.auth.claim_code = form.claim_code.value.trim();
  _configDoc.companion.auth.bearer_token = form.bearer_token.value.trim();
  cfg.webrtc = cfg.webrtc || {};
  cfg.webrtc.ice_servers = iceServers;
  renderHABadge(_configDoc.companion.auth.claimed);
  saveConfig(document.getElementById('haConfigStatus'), saveBtn).catch(function(err) { handleApiError(err); });
}

function renderIceServers(servers) {
  var list = document.getElementById('iceServersList');
  if (!list) return;
  var html = '';
  servers = servers || [];
  for (var i = 0; i < servers.length; i++) html += iceServerRowHTML(servers[i], false);
  html += iceServerRowHTML('', true);
  list.innerHTML = html;
}

function iceServerRowHTML(value, draft) {
  return '<div class="ice-server-row" data-draft="' + (draft ? 'true' : 'false') + '">'
    + '<div class="ice-server-field"><div class="ice-input-wrap"><input class="form-input ice-server-input" type="text" placeholder="e.g. stun:stun.l.google.com:19302" value="' + escapeHtml(value || '') + '" oninput="updateIceDraftButton(this)">'
    + (draft ? '<button type="button" class="ice-inline-btn ice-add-btn" onclick="commitIceServerRow(this)" style="display:none">Add</button>' : '<button type="button" class="ice-inline-btn ice-remove-btn" onclick="removeIceServerRow(this)">Remove</button>')
    + '</div><div class="form-hint">Enter a full ICE URL starting with stun:, turn:, or turns:</div></div>'
    + '</div>';
}

function updateIceDraftButton(input) {
  var row = input.closest('.ice-server-row');
  if (!row || row.getAttribute('data-draft') !== 'true') return;
  var btn = row.querySelector('.ice-add-btn');
  if (!btn) return;
  btn.style.display = input.value.trim() ? 'inline-block' : 'none';
}

function commitIceServerRow(btn) {
  var row = btn.closest('.ice-server-row');
  if (!row) return;
  var input = row.querySelector('.ice-server-input');
  var value = input.value.trim();
  var err = validateIceServer(value);
  if (err) {
    setStatus('haConfigStatus', err, 'var(--danger)');
    input.focus();
    return;
  }
  row.setAttribute('data-draft', 'false');
  btn.className = 'ice-inline-btn ice-remove-btn';
  btn.textContent = 'Remove';
  btn.removeAttribute('style');
  btn.setAttribute('onclick', 'removeIceServerRow(this)');
  setStatus('haConfigStatus', '');
  document.getElementById('iceServersList').insertAdjacentHTML('beforeend', iceServerRowHTML('', true));
}

function removeIceServerRow(btn) {
  var row = btn.closest('.ice-server-row');
  if (row) row.remove();
  if (!document.querySelector('#iceServersList .ice-server-row[data-draft="true"]')) {
    document.getElementById('iceServersList').insertAdjacentHTML('beforeend', iceServerRowHTML('', true));
  }
}

function collectIceServers() {
  var draft = document.querySelector('#iceServersList .ice-server-row[data-draft="true"] .ice-server-input');
  if (draft && draft.value.trim()) {
    setStatus('haConfigStatus', 'Click Add for the ICE server before saving.', 'var(--danger)');
    draft.focus();
    return null;
  }
  var inputs = document.querySelectorAll('#iceServersList .ice-server-row[data-draft="false"] .ice-server-input');
  var out = [];
  for (var i = 0; i < inputs.length; i++) {
    var value = inputs[i].value.trim();
    if (!value) continue;
    var err = validateIceServer(value);
    if (err) {
      setStatus('haConfigStatus', err, 'var(--danger)');
      inputs[i].focus();
      return null;
    }
    out.push(value);
  }
  return out;
}

function validateIceServer(value) {
  if (!value) return 'ICE server URL is required.';
  if (/\s/.test(value)) return 'ICE server URLs cannot contain spaces.';
  var match = value.match(/^(stun|turn|turns):((\[[0-9a-fA-F:.]+\])|([^/?#:]+))(?:\:([0-9]{1,5}))?(?:\?[^#\s]+)?$/i);
  if (!match) return 'Use stun:, turn:, or turns: with a hostname and optional port.';
  if (match[5]) {
    var port = Number(match[5]);
    if (port < 1 || port > 65535) return 'ICE server port must be between 1 and 65535.';
  }
  return '';
}

function renderEntrypointsConfig() {
  var cfg = ensureCompanionConfig();
  var list = cfg.entrypoints || [];
  var html = '';
  for (var i = 0; i < list.length; i++) html += entrypointCardHTML(list[i], i);
  document.getElementById('entrypointsList').innerHTML = html || '<div class="card dummy-content"><h2>No Entrypoints</h2><p>Add one to expose a gate.</p></div>';
  setStatus('entrypointsStatus', '');
}

function entrypointCardHTML(ep, idx) {
  ep = ep || {};
  return '<div class="card section entrypoint-card" data-index="' + idx + '">'
    + '<div class="flex-between mb-16"><div class="card-header" style="border:none;margin:0;padding:0">Entrypoint ' + (idx + 1) + '</div>'
    + '<button type="button" class="btn btn-danger btn-sm" onclick="removeEntrypointCard(' + idx + ')">Remove</button></div>'
    + '<div class="form-row">'
    + '<div class="form-group"><label class="form-label">ID</label><input class="form-input ep-id" type="text" value="' + escapeHtml(ep.id || '') + '" required></div>'
    + '<div class="form-group"><label class="form-label">Label</label><input class="form-input ep-label" type="text" value="' + escapeHtml(ep.label || '') + '" required></div>'
    + '</div>'
    + '<div class="form-group"><label class="form-label">Device Address</label><input class="form-input ep-devaddr" type="text" value="' + escapeHtml(ep.devaddr || ep.dev_addr || '') + '" required></div>'
    + '<div class="form-check-row">'
      + checkboxHTML('ep-stream-' + idx, 'ep-has-stream', 'Stream', !!(ep.capabilities && ep.capabilities.stream))
      + checkboxHTML('ep-unlock-' + idx, 'ep-has-unlock', 'Unlock', !!(ep.capabilities && ep.capabilities.unlock))
      + checkboxHTML('ep-ring-' + idx, 'ep-has-ring', 'Ring', !!(ep.capabilities && ep.capabilities.ring))
    + '</div>'
    + '</div>';
}

function checkboxHTML(id, cls, label, checked) {
  return '<div class="form-check"><input type="checkbox" class="' + cls + '" id="' + id + '"' + (checked ? ' checked' : '') + '><label for="' + id + '">' + label + '</label></div>';
}

function addEntrypointCard() {
  var cfg = ensureCompanionConfig();
  cfg.entrypoints = collectEntrypoints();
  cfg.entrypoints.push({ id: 'gate' + (cfg.entrypoints.length + 1), label: 'Gate ' + (cfg.entrypoints.length + 1), devaddr: '', capabilities: { stream: true, unlock: true, ring: true } });
  renderEntrypointsConfig();
}

function removeEntrypointCard(idx) {
  var cfg = ensureCompanionConfig();
  cfg.entrypoints = collectEntrypoints();
  cfg.entrypoints.splice(idx, 1);
  renderEntrypointsConfig();
}

function collectEntrypoints() {
  var cards = document.querySelectorAll('.entrypoint-card');
  var out = [];
  cards.forEach(function(card) {
    out.push({
      id: card.querySelector('.ep-id').value.trim(),
      label: card.querySelector('.ep-label').value.trim(),
      devaddr: card.querySelector('.ep-devaddr').value.trim(),
      capabilities: {
        stream: card.querySelector('.ep-has-stream').checked,
        unlock: card.querySelector('.ep-has-unlock').checked,
        ring: card.querySelector('.ep-has-ring').checked
      }
    });
  });
  return out;
}

function saveEntrypointsConfig(saveBtn) {
  var cfg = ensureCompanionConfig();
  cfg.entrypoints = collectEntrypoints();
  for (var i = 0; i < cfg.entrypoints.length; i++) {
    if (!cfg.entrypoints[i].id || !cfg.entrypoints[i].label || !cfg.entrypoints[i].devaddr) {
      setStatus('entrypointsStatus', 'ID, label, and device address are required.', 'var(--danger)');
      return;
    }
  }
  saveConfig(document.getElementById('entrypointsStatus'), saveBtn).catch(function(err) { handleApiError(err); });
}

function setStatus(id, msg, color) {
  var el = document.getElementById(id);
  if (!el) return;
  el.textContent = msg || '';
  el.style.color = color || '';
}

/* ──── Admin pages ──── */
function renderAdminUser() {
  var form = document.getElementById('adminUserForm');
  form.username.value = (_session && _session.username) || '';
  form.current_password.value = '';
  form.password.value = '';
  setStatus('adminUserStatus', '');
}

function saveAdminUser() {
  var form = document.getElementById('adminUserForm');
  apiPost('/webui/api/credentials', {
    username: form.username.value.trim(),
    current_password: form.current_password.value,
    password: form.password.value
  }).then(function() {
    showToast('Credentials saved. Sign in again.', 'success');
    _session = null;
    show('loginView');
    document.getElementById('loginForm').reset();
  }).catch(function(err) {
    setStatus('adminUserStatus', err.message || 'Save failed', 'var(--danger)');
    handleApiError(err);
  });
}

/* ──── About page ──── */
function renderAbout() {
  var cached = getCached('/webui/api/session');
  if (cached) renderAboutData(cached);
  apiGet('/webui/api/session', { cache: true }).then(renderAboutData).catch(function(err) { handleApiError(err); });
  var cachedStatus = getCached('/webui/api/status');
  if (cachedStatus) renderStatusData(cachedStatus);
  apiGet('/webui/api/status', { cache: true }).then(renderStatusData).catch(function(err) { handleApiError(err); });
  loadUpdateStatus();
}

function loadUpdateStatus() {
  apiGet('/webui/api/update/status').then(function(update) {
    renderUpdateStatus(update);
    if (update.update_available) showToast('Companion update ' + (update.latest_version || '') + ' is available.', 'info');
    if (update.restart_required) showToast('Companion update is staged and will activate on restart.', 'warning');
  }).catch(function(err) { handleApiError(err); });
}

function renderUpdateStatus(update) {
  var badge = document.getElementById('aboutUpdateBadge');
  if (!badge) return;
  var label = update.stage;
  if (update.stage === 'available') {
    label = update.latest_version || 'Available';
    badge.className = 'badge badge-info';
  } else if (update.stage === 'idle') {
    label = 'Up-to-date';
    badge.className = 'badge badge-success';
  } else if (update.stage === 'staged') {
    label = 'Restart required';
    badge.className = 'badge badge-info';
  } else if (update.stage === 'failed') {
    label = 'Update failed';
    badge.className = 'badge badge-danger';
  } else {
    badge.className = 'badge badge-info';
  }
  badge.textContent = label;
  var install = document.getElementById('aboutUpdateInstall');
  if (install) install.classList.toggle('hidden', !update.update_available);
}

function installUpdate() {
  var button = document.getElementById('aboutUpdateInstall');
  if (button) button.disabled = true;
  apiPost('/webui/api/update/install', {}).then(function(update) {
    if (!update.restart_required) {
      if (button) button.disabled = false;
      showToast('No update is available.', 'info');
      loadUpdateStatus();
      return;
    }
    showToast('Update installed. Companion is restarting...', 'success');
    setTimeout(pollRestartReady, 1500);
  }).catch(function(err) {
    if (button) button.disabled = false;
    handleApiError(err, 'Update failed');
  });
}

function renderAboutData(session) {
  if (!session) return;
  document.getElementById('aboutVersion').textContent = session.version || '-';
  document.getElementById('aboutGitSHA').textContent = session.git_sha && session.git_sha !== '-' ? session.git_sha.substring(0, 10) : '-';
}

function renderStatusData(status) {
  if (!status) return;
  document.getElementById('statusModel').textContent = status.model || '-';
  document.getElementById('statusFirmware').textContent = status.firmware || '-';
  document.getElementById('statusHardware').textContent = status.hardware || '-';
  document.getElementById('statusUptime').textContent = formatDuration(status.uptime_seconds);
  document.getElementById('statusFreeRAM').textContent = formatKB(status.free_ram_kb);
  document.getElementById('statusWifi').textContent = formatPercent(status.wifi_strength);

}

function renderDiagnostics() {
  apiGet('/webui/api/status').then(function(status) {
    var diagnostic = status.diagnostics || {};
    var own = diagnostic.openwebnet || {};
    var local = diagnostic.local || {};
    var fields = {
      diagIP: own.ip,
      diagNetmask: own.netmask,
      diagMAC: own.mac,
      diagFirmware: own.firmware,
      diagHardware: own.hardware,
      diagKernel: own.kernel,
      diagDistribution: own.distribution,
      diagInterface: local.interface,
      diagWiFi: formatPercent(local.wifi_strength),
      diagRefreshed: diagnostic.refreshed_at || '-',
      diagError: diagnostic.refresh_error || 'None'
    };
    Object.keys(fields).forEach(function(id) {
      var el = document.getElementById(id);
      if (el) el.textContent = fields[id] || '-';
    });
  }).catch(function(err) { handleApiError(err, 'Failed to load diagnostics'); });
}

function formatDuration(seconds) {
  seconds = Number(seconds || 0);
  if (!seconds || seconds < 0) return '-';
  var days = Math.floor(seconds / 86400);
  var hours = Math.floor((seconds % 86400) / 3600);
  var minutes = Math.floor((seconds % 3600) / 60);
  if (days > 0) return days + 'd ' + hours + 'h';
  if (hours > 0) return hours + 'h ' + minutes + 'm';
  return minutes + 'm';
}

function formatKB(kb) {
  kb = Number(kb || 0);
  if (!kb || kb < 0) return '-';
  if (kb >= 1024 * 1024) return (kb / 1024 / 1024).toFixed(1) + ' GB';
  return Math.round(kb / 1024) + ' MB';
}

function formatPercent(value) {
  if (value === null || value === undefined || value === '') return '-';
  var n = Number(value);
  if (isNaN(n)) return '-';
  return Math.round(n) + '%';
}

/* ──── Log / Frame page state ──── */
var _logPaused = false;
var _framePaused = false;
var _logTimer = null;
var _frameTimer = null;
var _logPrevContent = '';

function startPolling(page) {
  stopPolling();
  if (page === 'logs') { loadLoggingState(); fetchLogs(); _logTimer = setInterval(fetchLogs, 3000); }
  if (page === 'busframes') { fetchFrames(); _frameTimer = setInterval(fetchFrames, 2000); }
}

function stopPolling() {
  if (_logTimer) { clearInterval(_logTimer); _logTimer = null; }
  if (_frameTimer) { clearInterval(_frameTimer); _frameTimer = null; }
}

/* Patch switchPage to manage polling */
var _origSwitch = switchPage;
switchPage = function(pageId) {
  _origSwitch(pageId);
  if (pageId === 'logs' || pageId === 'busframes') {
    startPolling(pageId);
  } else {
    stopPolling();
  }
};

/* ──── Log viewer ──── */
function fetchLogs() {
  if (_logPaused) return;
  apiGet('/webui/api/logs').then(renderLogs).catch(function(err) { handleApiError(err, 'Failed to load logs'); });
}

function loadLoggingState() {
  apiGet('/webui/api/logging').then(function(data) {
    var runtimeLevel = document.getElementById('logRuntimeLevel');
    if (runtimeLevel && data.level) runtimeLevel.value = data.level;
  }).catch(function(err) { handleApiError(err, 'Failed to load logger state'); });
}

function setLoggingLevel(level) {
  apiPut('/webui/api/logging', { level: level }).then(function(data) {
    var runtimeLevel = document.getElementById('logRuntimeLevel');
    if (runtimeLevel && data.level) runtimeLevel.value = data.level;
    showToast('Log level set to ' + data.level.toUpperCase(), 'success');
  }).catch(function(err) {
    handleApiError(err, 'Failed to update logger level');
    loadLoggingState();
  });
}

function highlightText(text, query) {
  if (!query) return escapeHtml(text);
  var lower = text.toLowerCase();
  var needle = query.toLowerCase();
  var pos = 0;
  var out = '';
  while (true) {
    var idx = lower.indexOf(needle, pos);
    if (idx === -1) break;
    out += escapeHtml(text.substring(pos, idx));
    out += '<mark class="log-highlight">' + escapeHtml(text.substring(idx, idx + query.length)) + '</mark>';
    pos = idx + query.length;
  }
  return out + escapeHtml(text.substring(pos));
}

function renderLogs(data) {
  if (!data || !data.log) return;
  var pre = document.querySelector('#logOutput pre');
  var raw = data.log;
  if (raw === _logPrevContent) return;
  _logPrevContent = raw;
  var query = document.getElementById('logSearch').value.trim();
  var lines = raw.split('\n');
  var html = '';
  for (var i = 0; i < lines.length; i++) {
    var line = lines[i];
    if (!line) continue;
    var cls = 'log-line-info';
    if (/\[E\]/.test(line)) cls = 'log-line-error';
    else if (/\[W\]/.test(line)) cls = 'log-line-warn';
    else if (/\[D\]/.test(line)) cls = 'log-line-debug';
    html += '<span class="' + cls + '">' + highlightText(line, query) + '</span>\n';
  }
  pre.innerHTML = html;
  var out = document.getElementById('logOutput');
  out.scrollTop = out.scrollHeight;
}

function toggleLogPause() {
  _logPaused = !_logPaused;
  document.getElementById('logPauseBtn').textContent = _logPaused ? 'Resume' : 'Pause';
  if (!_logPaused) fetchLogs();
}

document.getElementById('logRuntimeLevel').addEventListener('change', function(event) {
  setLoggingLevel(event.target.value);
});

document.getElementById('logSearch').addEventListener('input', function() {
  _logPrevContent = '';
  fetchLogs();
});

/* ──── BUS Frame viewer ──── */
function fetchFrames() {
  if (_framePaused) return;
  apiGet('/webui/api/frames').then(renderFrames).catch(function(err) { handleApiError(err, 'Failed to load BUS frames'); });
}

function renderFrames(data) {
  if (!data || !data.frames) return;
  var container = document.getElementById('framesOutput');
  var frames = data.frames;
  if (frames.length === 0) {
    container.innerHTML = '<div class="frame-entry"><span class="frame-raw" style="color:#888">No frames captured yet.</span></div>';
    document.getElementById('frameCount').textContent = '0 frames';
    return;
  }
  var html = '';
  for (var i = 0; i < frames.length; i++) {
    var f = frames[i];
    var t = '';
    if (f.t) {
      var d = new Date(f.t);
      t = pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds());
    }
    var sys = f.sys || '?';
    var raw = f.raw || '';
    var mapped = f.mapped ? ' [' + f.events + ' evt]' : '';
    html += '<div class="frame-entry">'
      + '<span class="frame-time">' + t + '</span>'
      + '<span class="frame-system">' + escapeHtml(sys) + '</span>'
      + '<span class="frame-raw">' + escapeHtml(raw) + '</span>'
      + (mapped ? '<span class="frame-mapped">' + mapped + '</span>' : '')
      + '</div>';
  }
  container.innerHTML = html;
  container.scrollTop = container.scrollHeight;
  document.getElementById('frameCount').textContent = frames.length + ' frames';
}

function pad2(n) { return n < 10 ? '0' + n : String(n); }

function toggleFramePause() {
  _framePaused = !_framePaused;
  document.getElementById('framePauseBtn').textContent = _framePaused ? 'Resume' : 'Pause';
  if (!_framePaused) fetchFrames();
}

/* ──── Init ──── */
document.getElementById('loginForm').addEventListener('submit', submitLogin);
document.getElementById('setupForm').addEventListener('submit', submitSetup);

checkSession();

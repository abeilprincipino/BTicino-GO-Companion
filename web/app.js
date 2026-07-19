'use strict';

var _session = null;
var _configDoc = null;
var _sessionMonitor = null;
var _sessionRefreshPending = false;

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
        var message = data.error && typeof data.error === 'object' ? data.error.message : data.error;
        var err = new Error(message || r.statusText || 'Request failed');
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
        var message = data.error && typeof data.error === 'object' ? data.error.message : data.error;
        var err = new Error(message || r.statusText || 'Request failed');
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
    checkSession('expired');
    return;
  }
  if (fallback && _session && _session.authenticated) toast.show({ kind: 'error', message: fallback });
}

function getCached(url) {
  try {
    var raw = sessionStorage.getItem('cache:' + url);
    return raw ? JSON.parse(raw) : null;
  } catch (e) { return null; }
}

var toast = (function() {
  function region() {
    return document.getElementById('notificationRegion');
  }

  function dismiss(id) {
    var element = document.getElementById('notification-' + id);
    if (!element) return;
    if (element._timer) clearTimeout(element._timer);
    element.remove();
  }

  function show(options) {
    options = options || {};
    var scope = options.scope || 'authenticated';
    if (scope === 'authenticated' && (!_session || !_session.authenticated)) return;

    var kind = options.kind || 'success';
    var id = options.id || kind + '-' + options.message;
    dismiss(id);

    var element = document.createElement('div');
    element.id = 'notification-' + id;
    element.className = 'toast toast-' + kind;
    element.dataset.scope = scope;
    element.setAttribute('role', kind === 'error' ? 'alert' : 'status');

    var message = document.createElement('div');
    message.className = 'toast-message';
    message.textContent = options.message || '';
    element.appendChild(message);

    var action = document.createElement('button');
    action.type = 'button';
    action.className = 'toast-action';
    action.textContent = options.action ? options.action.label : '';
    action.disabled = !options.action;
    if (options.action) action.addEventListener('click', options.action.onClick);
    element.appendChild(action);

    var close = document.createElement('button');
    close.type = 'button';
    close.className = 'toast-close';
    close.setAttribute('aria-label', 'Dismiss notification');
    close.textContent = '×';
    close.disabled = options.dismissible === false;
    if (!close.disabled) close.addEventListener('click', function() { dismiss(id); });
    element.appendChild(close);

    region().appendChild(element);
    if (!options.persistent) {
      element._timer = setTimeout(function() { dismiss(id); }, options.duration || 3000);
    }
  }

  function clearScope(scope) {
    region().querySelectorAll('[data-scope="' + scope + '"]').forEach(function(element) {
      dismiss(element.id.replace('notification-', ''));
    });
  }

  function clearAll() {
    region().replaceChildren();
  }

  return { show: show, dismiss: dismiss, clearScope: clearScope, clearAll: clearAll };
})();

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
  if (pageId === 'configHa') renderHomeAssistantConfig();
  if (pageId === 'configEntrypoints') loadConfig().then(renderEntrypointsConfig).catch(function(err) { handleApiError(err, 'Failed to load config'); });
  if (pageId === 'configHomeKit') renderHomeKitConfig();
  if (pageId === 'managementSystem') renderSystemConfig();
  if (pageId === 'adminUser') renderAdminUser();
  startPolling(pageId);
}

/* ──── Nav bar ──── */
function buildNav() {
  var nav = document.getElementById('nav');
  if (!nav) return;
  var pages = [
    {
      label: 'Config',
      children: [
        { id: 'configEntrypoints', label: 'Entrypoints' },
        { id: 'configHa', label: 'Home Assistant' },
        { id: 'configHomeKit', label: 'HomeKit' }
      ]
    },
    {
      label: 'Management',
      children: [
        { id: 'managementSystem', label: 'System' },
        { id: 'diagnostics', label: 'Diagnostics' },
        { id: 'managementUpdates', label: 'Updates' }
      ]
    },
    {
      label: 'Logs',
      children: [
        { id: 'logs', label: 'Companion' },
        { id: 'busframes', label: 'OpenWebNet' }
      ]
    },
    { id: 'about', label: 'About' },
    {
      label: 'Admin',
      children: [
        { id: 'adminUser', label: 'User' },
        { action: 'restartCompanion()', label: 'Restart' },
        { action: 'rebootIntercom()', label: 'Reboot' },
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

function renderHomeKitConfig() {
  apiGet('/webui/api/config/homekit').then(function(data) {
    document.getElementById('homeKitEnabled').checked = !!data.enabled;
    setStatus('homeKitStatus', '');
  }).catch(function(err) { handleApiError(err, 'Failed to load HomeKit configuration'); });
}

function saveHomeKitConfig(button) {
  apiPut('/webui/api/config/homekit', { enabled: document.getElementById('homeKitEnabled').checked }).then(function() {
    setStatus('homeKitStatus', 'Saved. Restart Companion to apply changes.', 'var(--success)');
  }).catch(function(err) { handleApiError(err); });
}

function renderSystemConfig() {
  apiGet('/webui/api/management/system').then(function(data) {
    document.getElementById('systemRebootEnabled').checked = !!data.reboot_enabled;
    document.getElementById('systemUpdateEnabled').checked = !!data.update_enabled;
    document.getElementById('systemUpdateExposed').checked = !!data.update_exposed;
    setStatus('systemStatus', '');
  }).catch(function(err) { handleApiError(err, 'Failed to load system configuration'); });
}

function saveSystemConfig(button) {
  apiPut('/webui/api/management/system', {
    reboot_enabled: document.getElementById('systemRebootEnabled').checked,
    update_enabled: document.getElementById('systemUpdateEnabled').checked,
    update_exposed: document.getElementById('systemUpdateExposed').checked,
    services: { dropbear: { enabled: true, exposed: true } }
  }).then(function() { setStatus('systemStatus', 'Saved. Restart Companion to apply changes.', 'var(--success)'); }).catch(function(err) { handleApiError(err); });
}

function toggleVis(btn) {
  var inp = btn.previousElementSibling;
  var show = inp.type === 'password';
  inp.type = show ? 'text' : 'password';
  btn.style.opacity = show ? '1' : '0.4';
}

/* ──── Session ──── */
function checkSession(reason) {
  if (_sessionRefreshPending) return;
  _sessionRefreshPending = true;
  return apiGet('/webui/api/session', { cache: true }).then(function(session) {
    applySession(session, reason);
  }).catch(function(err) {
    enterLogin(null, 'connection_error', err.message);
  }).finally(function() {
    _sessionRefreshPending = false;
  });
}

function applySession(session, reason) {
  _session = session;
  if (!session.authenticated) {
    enterLogin(session, reason);
    return;
  }
  if (session.bootstrap) {
    stopPolling();
    toast.clearScope('authenticated');
    resetSetupForm();
    show('setupView');
    return;
  }
  startSessionMonitor();
  show('appView');
  buildNav();
  refreshPairingToast();
  switchPage('about');
}

function enterLogin(session, reason, detail) {
  _session = session;
  stopSessionMonitor();
  stopPolling();
  toast.clearScope('authenticated');
  renderLoginHelp(!!(session && session.bootstrap_required));
  resetLoginForm();
  setLoginNotice(reason, detail);
  show('loginView');
}

function setLoginNotice(reason, detail) {
  var notice = document.getElementById('loginNotice');
  notice.className = 'auth-notice hidden';
  notice.textContent = '';
  if (reason === 'account_created') {
    notice.textContent = 'Account created. Sign in with your new credentials.';
    notice.className = 'auth-notice success';
  } else if (reason === 'expired') {
    notice.textContent = 'Your session ended. Sign in again.';
    notice.className = 'auth-notice';
  } else if (reason === 'restart') {
    notice.textContent = 'Companion restarted. Sign in again.';
    notice.className = 'auth-notice';
  } else if (reason === 'connection_error') {
    notice.textContent = detail || 'Unable to connect to Companion.';
    notice.className = 'auth-notice error';
  }
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

function resetSetupForm() {
  var form = document.getElementById('setupForm');
  if (!form) return;
  form.reset();
  var error = document.getElementById('setupError');
  if (error) error.classList.remove('visible');
  var button = form.querySelector('.btn');
  if (!button) return;
  button.disabled = false;
  button.textContent = 'Create Account';
  updateBootstrapPasswordStatus();
}

function updateBootstrapPasswordStatus() {
  var form = document.getElementById('setupForm');
  if (!form) return;
  var password = form.elements.password.value;
  var confirmation = form.elements.password_confirm.value;
  var passwordInput = form.elements.password;
  var confirmationInput = form.elements.password_confirm;
  var passwordMark = document.getElementById('setupPasswordMark');
  var confirmationMark = document.getElementById('setupPasswordConfirmMark');

  passwordMark.className = 'field-mark';
  confirmationMark.className = 'field-mark';
  passwordInput.classList.toggle('invalid', !!password && password.length < 8);
  confirmationInput.classList.toggle('invalid', !!confirmation && password !== confirmation);
  if (password.length >= 8) passwordMark.className = 'field-mark valid';
  if (confirmation) {
    var matches = password === confirmation;
    if (matches) confirmationMark.className = 'field-mark valid';
  }
}

function startSessionMonitor() {
  if (_sessionMonitor) return;
  _sessionMonitor = setInterval(function() {
    apiGet('/webui/api/session').then(function(session) {
      if (session.authenticated) return;
      applySession(session, 'restart');
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

  if (form.get('password') !== form.get('password_confirm')) {
    errEl.textContent = 'Passwords do not match.';
    errEl.classList.add('visible');
    btn.disabled = false;
    btn.textContent = 'Create Account';
    return;
  }

  apiPost('/webui/api/bootstrap/account', {
    username: form.get('username'),
    password: form.get('password'),
    password_confirm: form.get('password_confirm'),
    current_password: ''
  }).then(function(data) {
    if (data.error) {
      errEl.textContent = data.error;
      errEl.classList.add('visible');
      btn.disabled = false;
      btn.textContent = 'Create Account';
      return;
    }
    resetLoginForm();
    checkSession('account_created');
  }).catch(function(err) {
    errEl.textContent = err.message || 'Connection error';
    errEl.classList.add('visible');
    btn.disabled = false;
    btn.textContent = 'Create Account';
  });
}

function logout() {
  apiPost('/webui/api/admin/logout', {}).then(function() {
    checkSession('manual');
  }).catch(function(err) {
    toast.show({ kind: 'error', message: err.message || 'Logout failed' });
  });
}

function restartCompanion() {
  requestConfirmation('Restart Companion?', 'You will need to sign in again.', 'Restart', function() {
  toast.show({ kind: 'attention', message: 'Restarting Companion...' });
  apiPost('/webui/api/admin/restart', { confirm: true }).then(function() {
    setTimeout(pollRestartReady, 1500);
  }).catch(function(err) {
    toast.show({ kind: 'error', message: err.message || 'Restart failed' });
  }); });
}

function rebootIntercom() {
  requestConfirmation('Reboot intercom?', 'Companion and panel services will be unavailable until it starts again.', 'Reboot', function() {
  apiPost('/webui/api/admin/reboot', { confirm: true }).then(function() {
    toast.show({ kind: 'attention', message: 'Intercom reboot requested.' });
  }).catch(function(err) { toast.show({ kind: 'error', message: err.message || 'Reboot failed' }); }); });
}

var _confirmationAction = null;
function requestConfirmation(title, message, acceptLabel, action) {
  _confirmationAction = action;
  document.getElementById('confirmationTitle').textContent = title;
  document.getElementById('confirmationMessage').textContent = message;
  document.getElementById('confirmationAccept').textContent = acceptLabel;
  document.getElementById('confirmationDialog').classList.remove('hidden');
}
function closeConfirmation() {
  _confirmationAction = null;
  document.getElementById('confirmationDialog').classList.add('hidden');
}
function acceptConfirmation() {
  var action = _confirmationAction;
  closeConfirmation();
  if (action) action();
}

function pollRestartReady() {
  var attempts = 0;
  if (_restartTimer) clearInterval(_restartTimer);
  _restartTimer = setInterval(function() {
    attempts++;
    apiGet('/webui/api/session').then(function(session) {
      clearInterval(_restartTimer);
      _restartTimer = null;
      applySession(session, 'restart');
    }).catch(function() {
      if (attempts >= 30) {
        clearInterval(_restartTimer);
        _restartTimer = null;
        toast.show({ kind: 'attention', message: 'Restart is taking longer than expected. Refresh the page soon.' });
      }
    });
  }, 1000);
}

/* ──── Config pages ──── */
function loadConfig() {
  return apiGet('/webui/api/config/entrypoints').then(function(data) {
    _configDoc = data;
    return _configDoc;
  });
}

function saveConfig(statusEl, saveBtn) {
  if (!_configDoc) return Promise.reject(new Error('Config is not loaded'));
  return apiPut('/webui/api/config/entrypoints', _configDoc).then(function() {
    if (statusEl) setStatus(statusEl.id, '');
    flashSavedButton(saveBtn);
    toast.show({ kind: 'attention', message: 'Saved. Restart Companion to apply changes.' });
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
  return _configDoc;
}

function renderHomeAssistantConfig() {
  apiGet('/webui/api/config/homeassistant').then(function(pairing) {
    renderPairingStatus(pairing);
  }).catch(function(err) { handleApiError(err, 'Failed to load pairing status'); });
}

function renderPairingStatus(pairing) {
  var state = pairing.pairing_state || 'error';
  var heading = document.getElementById('haConfigHeading');
  var description = document.getElementById('haConfigDescription');
  var claim = document.getElementById('haConfigClaim');
  var recovery = document.getElementById('haConfigRecovery');
  var recoveryButton = document.getElementById('haConfigRecoveryButton');
  document.getElementById('haConfigModel').textContent = pairing.model || pairing.device_id || '-';
  claim.classList.add('hidden');
  recovery.classList.add('hidden');
  recoveryButton.classList.add('hidden');
  renderHABadge(state);
  renderPairingToast(pairing);

  if (state === 'claimable') {
    heading.textContent = 'Ready to connect';
    description.textContent = 'Install the BTicino Companion integration in Home Assistant, select this discovered Companion, and enter the claim code below.';
    document.getElementById('haConfigClaimCode').textContent = pairing.claim_code || '-';
    claim.classList.remove('hidden');
    setStatus('haConfigStatus', 'Waiting for Home Assistant to pair this Companion.');
    return;
  }
  if (state === 'claimed') {
    heading.textContent = 'Connected to Home Assistant';
    description.textContent = 'Home Assistant is authorized to control this Companion.';
    setStatus('haConfigStatus', 'Generate a recovery code only when Home Assistant asks you to reconnect it.');
    recoveryButton.classList.remove('hidden');
    if (pairing.recovery_code) {
      document.getElementById('haConfigRecoveryCode').textContent = pairing.recovery_code;
      document.getElementById('haConfigRecoveryExpiry').textContent = pairing.recovery_code_expires ? 'Expires ' + new Date(pairing.recovery_code_expires).toLocaleTimeString() + '.' : '';
      recovery.classList.remove('hidden');
    }
    return;
  }
  if (state === 'setup_required') {
    heading.textContent = 'Owner setup required';
    description.textContent = 'Sign in with the default credentials and create a real Companion owner account before pairing Home Assistant.';
    setStatus('haConfigStatus', 'Complete owner setup before requesting a claim code.');
    return;
  }
  heading.textContent = 'Pairing needs attention';
  description.textContent = 'Companion pairing configuration is not valid.';
  setStatus('haConfigStatus', 'Restart Companion or review its configuration before pairing.', 'var(--danger)');
}

function renderHABadge(state) {
  var badge = document.getElementById('haConfigBadge');
  if (!badge) return;
  var labels = { claimable: 'Ready to connect', claimed: 'Connected', setup_required: 'Setup required', error: 'Configuration error' };
  badge.textContent = labels[state] || 'Unknown';
  badge.className = 'badge ' + (state === 'claimed' ? 'badge-success' : state === 'error' ? 'badge-danger' : 'badge-info');
}

function issueRepairCode() {
  apiPost('/webui/api/config/homeassistant/recovery-code', {}).then(function(data) {
    toast.show({ kind: 'success', message: 'Recovery code generated.' });
    renderHomeAssistantConfig();
  }).catch(function(err) { handleApiError(err, 'Failed to issue repair code'); });
}

function copyPairingCode(elementID) {
  var code = document.getElementById(elementID).textContent.trim();
  if (!code || code === '-') return;
  var copied = navigator.clipboard && window.isSecureContext ? navigator.clipboard.writeText(code) : Promise.reject(new Error('clipboard unavailable'));
  copied.catch(function() {
    var selection = window.getSelection();
    var range = document.createRange();
    range.selectNodeContents(document.getElementById(elementID));
    selection.removeAllRanges();
    selection.addRange(range);
    if (!document.execCommand('copy')) throw new Error('copy failed');
    selection.removeAllRanges();
  }).then(function() {
    toast.show({ kind: 'success', message: 'Code copied.' });
  }).catch(function() {
    toast.show({ kind: 'error', message: 'Copy the code manually.' });
  });
}

function refreshPairingToast() {
  apiGet('/webui/api/config/homeassistant').then(function(pairing) {
    renderPairingToast(pairing);
  }).catch(function(err) { handleApiError(err); });
}

function renderPairingToast(pairing) {
  if (!_session || !_session.authenticated) return;
  if (pairing.pairing_state === 'claimable') {
    toast.show({
      id: 'pairing-ready',
      kind: 'success',
      message: 'Companion ready to be paired!',
      action: { label: 'Open Home Assistant', onClick: function() { switchPage('configHa'); } },
      scope: 'authenticated',
      persistent: true,
      dismissible: false
    });
    return;
  }
  toast.dismiss('pairing-ready');
  if (pairing.pairing_state === 'error') {
    toast.show({
      id: 'pairing-error',
      kind: 'error',
      message: 'Companion pairing needs attention.',
      action: { label: 'Review pairing', onClick: function() { switchPage('configHa'); } },
      scope: 'authenticated',
      persistent: true,
      dismissible: false
    });
  } else {
    toast.dismiss('pairing-error');
  }
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
  var removeDisabled = ensureCompanionConfig().entrypoints.length <= 1;
  return '<div class="card section entrypoint-card" data-index="' + idx + '">'
    + '<div class="flex-between mb-16"><div class="card-header" style="border:none;margin:0;padding:0">Entrypoint ' + (idx + 1) + '</div>'
    + '<button type="button" class="btn btn-danger btn-sm"' + (removeDisabled ? ' disabled title="At least one entrypoint is required"' : '') + ' onclick="removeEntrypointCard(' + idx + ')">Remove</button></div>'
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
  if (cfg.entrypoints.length <= 1) {
    setStatus('entrypointsStatus', 'At least one entrypoint is required.', 'var(--danger)');
    return;
  }
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
  if (!cfg.entrypoints.length) {
    setStatus('entrypointsStatus', 'At least one entrypoint is required.', 'var(--danger)');
    return;
  }
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
  apiPost('/webui/api/admin/account', {
    username: form.username.value.trim(),
    current_password: form.current_password.value,
    password: form.password.value
  }).then(function() {
    toast.show({ kind: 'success', message: 'Credentials saved. Sign in again.' });
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
  var cachedStatus = getCached('/webui/api/management/diagnostics');
  if (cachedStatus) renderStatusData(cachedStatus);
  apiGet('/webui/api/management/diagnostics', { cache: true }).then(renderStatusData).catch(function(err) { handleApiError(err); });
  loadUpdateStatus();
}

function loadUpdateStatus() {
  apiGet('/webui/api/management/update').then(function(update) {
    renderUpdateStatus(update);
    if (update.update_available) toast.show({ id: 'update-available', kind: 'attention', message: 'Companion update ' + (update.latest_version || '') + ' is available.' });
    if (update.restart_required) toast.show({ id: 'update-restart', kind: 'attention', message: 'Companion update is staged and will activate on restart.' });
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
  apiPost('/webui/api/management/update', {}).then(function(update) {
    if (!update.restart_required) {
      if (button) button.disabled = false;
      toast.show({ kind: 'success', message: 'No update is available.' });
      loadUpdateStatus();
      return;
    }
    toast.show({ kind: 'success', message: 'Update installed. Companion is restarting...' });
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
  apiGet('/webui/api/management/diagnostics').then(function(status) {
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
var _pairingTimer = null;
var _restartTimer = null;
var _logPrevContent = '';

function startPolling(page) {
  stopPolling();
  if (page === 'logs') { loadLoggingState(); fetchLogs(); _logTimer = setInterval(fetchLogs, 3000); }
  if (page === 'busframes') { fetchFrames(); _frameTimer = setInterval(fetchFrames, 2000); }
  if (page === 'configHa') { _pairingTimer = setInterval(renderHomeAssistantConfig, 3000); }
}

function stopPolling() {
  if (_logTimer) { clearInterval(_logTimer); _logTimer = null; }
  if (_frameTimer) { clearInterval(_frameTimer); _frameTimer = null; }
  if (_pairingTimer) { clearInterval(_pairingTimer); _pairingTimer = null; }
  if (_restartTimer) { clearInterval(_restartTimer); _restartTimer = null; }
}

/* ──── Log viewer ──── */
function fetchLogs() {
  if (_logPaused) return;
  apiGet('/webui/api/logs/companion').then(renderLogs).catch(function(err) { handleApiError(err, 'Failed to load logs'); });
}

function loadLoggingState() {
  apiGet('/webui/api/logs/companion/level').then(function(data) {
    var runtimeLevel = document.getElementById('logRuntimeLevel');
    if (runtimeLevel && data.level) runtimeLevel.value = data.level;
  }).catch(function(err) { handleApiError(err, 'Failed to load logger state'); });
}

function setLoggingLevel(level) {
  apiPut('/webui/api/logs/companion/level', { level: level }).then(function(data) {
    var runtimeLevel = document.getElementById('logRuntimeLevel');
    if (runtimeLevel && data.level) runtimeLevel.value = data.level;
    toast.show({ kind: 'success', message: 'Log level set to ' + data.level.toUpperCase() + '.' });
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
  apiGet('/webui/api/logs/openwebnet').then(renderFrames).catch(function(err) { handleApiError(err, 'Failed to load BUS frames'); });
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
document.querySelector('#setupForm [name="password"]').addEventListener('input', updateBootstrapPasswordStatus);
document.querySelector('#setupForm [name="password_confirm"]').addEventListener('input', updateBootstrapPasswordStatus);

checkSession();

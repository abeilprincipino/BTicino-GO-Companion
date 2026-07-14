'use strict';

var currentConfig;

function request(path, options) {
  options = options || {};
  return fetch('/webui/api/' + path, {
    method: options.method || 'GET',
    credentials: 'same-origin',
    headers: options.body ? { 'Content-Type': 'application/json' } : undefined,
    body: options.body ? JSON.stringify(options.body) : undefined
  }).then(function(response) {
    return response.json().catch(function() { return {}; }).then(function(data) {
      if (!response.ok) throw new Error(data.error || 'Request failed');
      return data;
    });
  });
}

function toast(message, type, action) {
  var element = document.getElementById('toast');
  element.className = 'toast ' + (type || 'info');
  element.textContent = message;
  if (action) {
    var button = document.createElement('button');
    button.textContent = action.label;
    button.onclick = action.run;
    element.appendChild(button);
  }
  if (type !== 'warning') setTimeout(function() { element.className = ''; }, 3500);
}

function show(id) {
  ['login', 'password', 'app'].forEach(function(view) {
    document.getElementById(view).classList.toggle('hidden', view !== id);
  });
}

function loadConfig() {
  return request('config').then(function(config) {
    currentConfig = config;
    var form = document.getElementById('config-form');
    form.name.value = config.companion.name;
    form.log_level.value = config.companion.log_level;
    form.reboot_enabled.checked = config.system.reboot_enabled;
    form.update_enabled.checked = config.system.update_enabled;
    form.update_exposed.checked = config.system.update_exposed;
    form.allow_rollback.checked = config.system.allow_rollback;
    form.homekit_enabled.checked = config.homekit.enabled;
  });
}

function restart() {
  request('restart', { method: 'POST', body: {} }).then(function() {
    toast('Companion is restarting.', 'info');
  }).catch(function(error) { toast(error.message, 'error'); });
}

document.getElementById('login-form').addEventListener('submit', function(event) {
  event.preventDefault();
  var form = new FormData(event.target);
  request('login', { method: 'POST', body: { username: form.get('username'), password: form.get('password') } }).then(function(result) {
    if (result.password_change_required) return show('password');
    show('app');
    return loadConfig();
  }).catch(function(error) { toast(error.message, 'error'); });
});

document.getElementById('password-form').addEventListener('submit', function(event) {
  event.preventDefault();
  var form = new FormData(event.target);
  request('password', { method: 'POST', body: { password: form.get('password') } }).then(function() {
    toast('Password changed. Sign in again.', 'success');
    show('login');
  }).catch(function(error) { toast(error.message, 'error'); });
});

document.getElementById('config-form').addEventListener('submit', function(event) {
  event.preventDefault();
  var form = event.target;
  currentConfig.companion.name = form.name.value;
  currentConfig.companion.log_level = form.log_level.value;
  currentConfig.system.reboot_enabled = form.reboot_enabled.checked;
  currentConfig.system.update_enabled = form.update_enabled.checked;
  currentConfig.system.update_exposed = form.update_exposed.checked;
  currentConfig.system.allow_rollback = form.allow_rollback.checked;
  currentConfig.homekit.enabled = form.homekit_enabled.checked;
  request('config', { method: 'PUT', body: currentConfig }).then(function(result) {
    if (result.restart_required) toast('Configuration saved. Restart required.', 'warning', { label: 'Restart now', run: restart });
  }).catch(function(error) { toast(error.message, 'error'); });
});

document.getElementById('logout').addEventListener('click', function() {
  request('logout', { method: 'POST', body: {} }).finally(function() { show('login'); });
});

request('session').then(function(session) {
  if (!session.authenticated) return show('login');
  if (session.password_change_required) return show('password');
  show('app');
  return loadConfig();
}).catch(function(error) { toast(error.message, 'error'); });

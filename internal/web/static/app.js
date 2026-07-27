// Shared UI helpers. The rest of the interactivity is htmx attributes in the
// templates, so this file stays small on purpose.

// Every destructive action goes through the same confirmation dialog.
function confirmAction(message, onConfirm) {
  const dialog = document.getElementById('confirm-dialog');
  if (!dialog || typeof dialog.showModal !== 'function') {
    if (window.confirm(message)) onConfirm();
    return;
  }

  document.getElementById('confirm-message').textContent = message;

  const handler = () => {
    dialog.removeEventListener('close', handler);
    if (dialog.returnValue === 'ok') onConfirm();
  };
  dialog.addEventListener('close', handler);
  dialog.showModal();
}

// The FTP browser feeds the "add library" form.
function selectFtpPath(path) {
  const input = document.getElementById('ftp-path-input');
  if (!input) return;
  input.value = path;

  const nameInput = document.querySelector('input[name="name"]');
  if (nameInput && !nameInput.value) {
    nameInput.value = path.split('/').filter(Boolean).pop() || '';
  }
  input.scrollIntoView({ behavior: 'smooth', block: 'center' });
}

// This script tag lives in the body, and hx-boost swaps the whole body, so
// htmx re-runs the file on every boosted navigation. The listeners below are
// bound to <body>, which survives that swap: without this guard each
// navigation would stack another copy and fire every toast twice.
if (!window.__proxystreamWired) {
window.__proxystreamWired = true;

// htmx swaps in place and leaves the scroll where it was, so a folder opened
// from the bottom of a long list starts halfway down. This event fires only on
// forward navigation, which leaves the back button free to restore its scroll.
document.body.addEventListener('htmx:pushedIntoHistory', () => {
  window.scrollTo({ top: 0, left: 0, behavior: 'instant' });
});

// ---------- toasts ----------

// Actions answer in a fragment somewhere on the page, which is easy to miss and
// sometimes off screen entirely. Every outcome also surfaces here.
function toast(message, level = 'info', timeout = 5000) {
  const host = document.getElementById('toasts');
  if (!host || !message) return;

  const el = document.createElement('div');
  el.className = 'toast toast-' + level;
  el.setAttribute('role', level === 'error' ? 'alert' : 'status');

  const text = document.createElement('span');
  text.className = 'toast-text';
  text.textContent = message;

  const close = document.createElement('button');
  close.type = 'button';
  close.className = 'toast-close';
  close.title = 'Fermer';
  close.setAttribute('aria-label', 'Fermer');
  close.textContent = '✕';

  const dismiss = () => {
    el.classList.add('is-leaving');
    el.addEventListener('transitionend', () => el.remove(), { once: true });
    // A dropped transition must not leave the toast on screen for ever.
    setTimeout(() => el.remove(), 400);
  };
  close.addEventListener('click', dismiss);

  el.append(text, close);
  host.append(el);
  requestAnimationFrame(() => el.classList.add('is-shown'));
  if (timeout > 0) setTimeout(dismiss, timeout);
}

// A handler answering with HX-Refresh replaces the page immediately, so its
// message has to cross the reload.
const PENDING_TOAST = 'pending-toast';

document.body.addEventListener('toast', (event) => {
  const d = event.detail || {};
  if (!d.message) return;
  if (d.reload) {
    try {
      sessionStorage.setItem(PENDING_TOAST, JSON.stringify(d));
    } catch (e) {
      toast(d.message, d.level);
    }
    return;
  }
  toast(d.message, d.level);
});

function drainPendingToast() {
  let raw = null;
  try {
    raw = sessionStorage.getItem(PENDING_TOAST);
    sessionStorage.removeItem(PENDING_TOAST);
  } catch (e) {
    return;
  }
  if (!raw) return;
  try {
    const d = JSON.parse(raw);
    toast(d.message, d.level);
  } catch (e) {
    /* nothing usable stored */
  }
}
drainPendingToast();

// ---------- request progress ----------

// A swap can take a while against a remote FTP, and htmx leaves the old content
// in place meanwhile. Without this the app looks frozen after a click.
let inFlight = 0;

// Pollers fire on their own every few seconds; flashing the bar for those would
// make it meaningless.
function isPolling(elt) {
  const trigger = elt && elt.getAttribute && elt.getAttribute('hx-trigger');
  return !!trigger && trigger.includes('every ');
}

function setProgress(active) {
  const bar = document.getElementById('progress');
  if (bar) bar.classList.toggle('is-active', active);
}

function startProgress() {
  inFlight += 1;
  setProgress(true);
}

function endProgress() {
  inFlight = Math.max(0, inFlight - 1);
  if (inFlight === 0) setProgress(false);
}

document.body.addEventListener('htmx:beforeRequest', (event) => {
  if (isPolling(event.detail.elt)) return;
  startProgress();
});

document.body.addEventListener('htmx:afterRequest', (event) => {
  if (isPolling(event.detail.elt)) return;
  endProgress();
});

// A plain form or link leaves the page entirely: keep the bar running until the
// browser paints the next one.
document.addEventListener('submit', (event) => {
  const form = event.target;
  if (!(form instanceof HTMLFormElement) || form.hasAttribute('hx-post')) return;
  if (form.method && form.method.toLowerCase() === 'dialog') return;
  startProgress();
  const submitter = event.submitter;
  if (submitter && submitter.classList.contains('btn')) {
    submitter.classList.add('is-busy');
  }
});

// Coming back through the bfcache must not restore a bar stuck mid-run.
window.addEventListener('pageshow', () => {
  inFlight = 0;
  setProgress(false);
  document.querySelectorAll('.btn.is-busy').forEach((b) => b.classList.remove('is-busy'));
});

// ---------- failures ----------

document.body.addEventListener('htmx:responseError', (event) => {
  const status = event.detail.xhr.status;
  // A session that expired mid-navigation should land on the login page rather
  // than silently swapping in an error fragment.
  if (status === 401) {
    window.location.href = '/login';
    return;
  }
  toast(
    status === 404
      ? "Cette page n'existe plus."
      : 'Le serveur a répondu ' + status + '. Rien n’a été modifié.',
    'error',
  );
});

// htmx drops a request whose hx-target matches nothing: no call, no error, no
// trace. A dead button is indistinguishable from a slow one, which is how the
// scan and refresh buttons went unnoticed. Never let it be silent again.
document.body.addEventListener('htmx:targetError', (event) => {
  const target = event.detail && event.detail.target;
  console.error('htmx: missing target', target, event.detail && event.detail.elt);
  toast(
    'Ce bouton pointe vers un élément absent de la page (' + target + '). ' +
      "L'action n'a pas été envoyée.",
    'error',
    0,
  );
});

document.body.addEventListener('htmx:sendError', () => {
  toast('Serveur injoignable. Vérifiez que Proxystream tourne toujours.', 'error');
});

document.body.addEventListener('htmx:timeout', () => {
  toast('Le serveur met trop de temps à répondre.', 'error');
});

} // end one-time wiring

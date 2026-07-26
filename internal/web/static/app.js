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

// A session that expired mid-navigation should land on the login page rather
// than silently swapping in an error fragment.
document.body.addEventListener('htmx:responseError', (event) => {
  if (event.detail.xhr.status === 401) {
    window.location.href = '/login';
  }
});

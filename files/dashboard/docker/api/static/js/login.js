document.addEventListener('DOMContentLoaded', () => {
  const f = document.getElementById('f');
  if (!f) return;
  const btn = f.querySelector('button');
  const err = document.getElementById('err');

  f.addEventListener('submit', async (e) => {
    e.preventDefault();
    err.hidden = true;
    btn.disabled = true;
    const label = btn.textContent;
    btn.textContent = 'Signing in…';
    try {
      await api.post('/api/login', {
        username: document.getElementById('u').value,
        password: document.getElementById('p').value,
      });
      location.href = '/app';
    } catch (ex) {
      err.textContent = ex.message;
      err.hidden = false;
      btn.disabled = false;
      btn.textContent = label;
      document.getElementById('p').select();
    }
  });
});

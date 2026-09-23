(() => {
  'use strict';
  const root = document.documentElement;
  const theme = document.querySelector('#theme-toggle');
  const applyTheme = value => { root.dataset.theme = value; theme.textContent = value === 'dark' ? 'Light' : 'Dark'; theme.setAttribute('aria-label', `Switch to ${value === 'dark' ? 'light' : 'dark'} theme`); };
  let saved; try { saved = localStorage.getItem('gvpc-theme'); } catch (_) {}
  applyTheme(saved || (window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'));
  theme.addEventListener('click', () => { const value = root.dataset.theme === 'dark' ? 'light' : 'dark'; applyTheme(value); try { localStorage.setItem('gvpc-theme', value); } catch (_) {} });
  const menu = document.querySelector('#menu-toggle');
  const sidebar = document.querySelector('#sidebar');
  const setMenu = value => { sidebar.classList.toggle('is-open', value); menu.setAttribute('aria-expanded', String(value)); };
  menu.addEventListener('click', () => setMenu(menu.getAttribute('aria-expanded') !== 'true'));
  const search = document.querySelector('#search');
  const results = document.querySelector('#search-results');
  const status = document.querySelector('#search-status');
  const normalize = value => value.normalize('NFKC').toLowerCase();
  search.addEventListener('input', () => {
    const query = normalize(search.value.trim());
    results.replaceChildren(); results.hidden = !query;
    if (!query) { status.textContent = ''; return; }
    const terms = query.split(/\s+/);
    const matches = (window.GVPC_SEARCH || []).map(page => {
      const text = normalize(page.title + ' ' + page.text);
      return {page, score: terms.every(term => text.includes(term)) ? terms.reduce((n,term) => n + (normalize(page.title).includes(term) ? 10 : 1), 0) : 0};
    }).filter(item => item.score).sort((a,b) => b.score - a.score).slice(0, 10);
    status.textContent = `${matches.length} matching pages`;
    if (!matches.length) { const empty = document.createElement('p'); empty.textContent = 'No matching pages. Try “gateway”, “BFD” or “installation”.'; results.append(empty); }
    matches.forEach(({page}) => {
      const link = document.createElement('a'); link.href = page.url;
      const title = document.createElement('strong'); title.textContent = page.title;
      const snippet = document.createElement('span'); const index = Math.max(0, normalize(page.text).indexOf(terms[0]) - 40); snippet.textContent = (index ? '…' : '') + page.text.slice(index, index + 160) + '…';
      link.append(title, snippet); results.append(link);
    });
  });
  document.addEventListener('keydown', event => {
    if (event.key === '/' && !['INPUT','TEXTAREA'].includes(document.activeElement.tagName)) { event.preventDefault(); if (window.innerWidth <= 780) setMenu(true); search.focus(); }
    if (event.key === 'Escape') { if (search.value) { search.value = ''; search.dispatchEvent(new Event('input')); } else { setMenu(false); menu.focus(); } }
  });
  document.querySelectorAll('.code-block').forEach(block => {
    if (!navigator.clipboard) return;
    const button = document.createElement('button'); button.type = 'button'; button.className = 'copy-code'; button.textContent = 'Copy'; button.setAttribute('aria-label', 'Copy code block');
    button.addEventListener('click', async () => { try { await navigator.clipboard.writeText(block.querySelector('code').textContent); button.textContent = 'Copied'; setTimeout(() => { button.textContent = 'Copy'; }, 1800); } catch (_) { button.textContent = 'Select code to copy'; } });
    block.prepend(button);
  });
})();

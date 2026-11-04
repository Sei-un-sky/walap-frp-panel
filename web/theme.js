(() => {
  const key = 'frp-manager-theme';
  const media = window.matchMedia('(prefers-color-scheme: dark)');

  function mode() {
    return localStorage.getItem(key) || 'auto';
  }

  function apply() {
    const selected = mode();
    document.documentElement.dataset.theme = selected === 'auto' ? (media.matches ? 'dark' : 'light') : selected;
    document.documentElement.dataset.themeMode = selected;
    const button = document.querySelector('#theme-toggle');
    if (button) {
      button.textContent = selected === 'auto' ? '◐' : selected === 'dark' ? '☀' : '☾';
      button.title = selected === 'auto' ? '外观：跟随系统（点击切换）' : selected === 'dark' ? '外观：夜间（点击切换）' : '外观：明亮（点击切换）';
      button.setAttribute('aria-label', button.title);
    }
  }

  function cycle() {
    const next = { auto: 'dark', dark: 'light', light: 'auto' }[mode()];
    if (next === 'auto') localStorage.removeItem(key);
    else localStorage.setItem(key, next);
    apply();
  }

  media.addEventListener('change', () => {
    if (mode() === 'auto') apply();
  });

  window.frpTheme = { apply, cycle };
  apply();
})();

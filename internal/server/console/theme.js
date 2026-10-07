// Runs before the stylesheet so a remembered theme is applied before first paint.
(() => {
  "use strict";
  const storageKey = "workbuddy.console.theme";
  const themes = [
    {id: "default", name: "默认", description: "经典浅色 · 清爽专注"},
    {id: "mint", name: "薄荷微光", description: "流动微光 · 轻盈呼吸"},
    {id: "grid", name: "精密网格", description: "细线网格 · 有序律动"},
    {id: "soft", name: "柔和几何", description: "圆弧色块 · 温柔漂浮"},
  ];
  const root = document.documentElement;
  const normalize = value => themes.some(theme => theme.id === value) ? value : "default";
  let selected = "default";
  try { selected = normalize(localStorage.getItem(storageKey)); } catch (_) { /* Storage may be disabled. */ }
  root.dataset.theme = selected;

  document.addEventListener("DOMContentLoaded", () => {
    const account = document.querySelector(".topbar .account-entry");
    if (!account) return;
    const picker = document.createElement("div");
    picker.className = "theme-picker";
    picker.innerHTML = `<button type="button" id="themeToggle" class="theme-toggle" aria-haspopup="menu" aria-expanded="false" aria-controls="themeMenu">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true"><path d="M12 3a9 9 0 1 0 0 18h1.3a2.2 2.2 0 0 0 1.4-3.9 1.5 1.5 0 0 1 1-2.6H17a4 4 0 0 0 4-4C21 6.3 17 3 12 3Z"/><circle cx="7.5" cy="10" r=".8"/><circle cx="11" cy="7" r=".8"/><circle cx="15.5" cy="8" r=".8"/></svg>
      <span>主题</span><svg class="theme-chevron" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" aria-hidden="true"><path d="m7 10 5 5 5-5"/></svg>
    </button><div id="themeMenu" class="theme-menu" role="menu" aria-label="选择主题" hidden>
      <div class="theme-menu-title" role="presentation">外观主题 <span>即时切换</span></div>
      ${themes.map(theme => `<button type="button" class="theme-option" role="menuitemradio" aria-checked="false" data-theme-choice="${theme.id}" tabindex="-1"><span class="theme-swatch theme-swatch-${theme.id}" aria-hidden="true"><i></i><i></i><i></i></span><span class="theme-option-copy"><strong>${theme.name}</strong><small>${theme.description}</small></span><span class="theme-check" aria-hidden="true">✓</span></button>`).join("")}
      <div class="theme-menu-note" role="presentation">自动记住此浏览器的选择</div>
    </div>`;
    account.before(picker);
    const toggle = picker.querySelector("#themeToggle");
    const menu = picker.querySelector("#themeMenu");
    const options = [...picker.querySelectorAll("[data-theme-choice]")];
    const decoration = document.createElement("div");
    decoration.className = "theme-backdrop";
    decoration.setAttribute("aria-hidden", "true");
    decoration.innerHTML = "<span></span><span></span>";
    document.body.prepend(decoration);

    function sync() {
      root.dataset.theme = selected;
      const name = themes.find(theme => theme.id === selected).name;
      toggle.setAttribute("aria-label", "切换主题，当前：" + name);
      toggle.title = "当前主题：" + name;
      options.forEach(option => option.setAttribute("aria-checked", String(option.dataset.themeChoice === selected)));
    }
    function close(restoreFocus = false) {
      menu.hidden = true;
      toggle.setAttribute("aria-expanded", "false");
      if (restoreFocus) toggle.focus();
    }
    function open(index = options.findIndex(option => option.dataset.themeChoice === selected)) {
      menu.hidden = false;
      toggle.setAttribute("aria-expanded", "true");
      options[index].focus();
    }
    toggle.addEventListener("click", () => menu.hidden ? open() : close());
    toggle.addEventListener("keydown", event => {
      if (event.key === "ArrowDown" || event.key === "ArrowUp") {
        event.preventDefault();
        event.stopPropagation();
        open(event.key === "ArrowDown" ? 0 : options.length - 1);
      }
    });
    options.forEach(option => option.addEventListener("click", () => {
      selected = normalize(option.dataset.themeChoice);
      try { localStorage.setItem(storageKey, selected); } catch (_) { /* Switching still works without persistence. */ }
      sync();
      close(true);
    }));
    picker.addEventListener("keydown", event => {
      if (menu.hidden) return;
      if (event.key === "Escape") { event.preventDefault(); close(true); return; }
      const index = options.indexOf(document.activeElement);
      let next;
      if (event.key === "ArrowDown") next = (index + 1) % options.length;
      if (event.key === "ArrowUp") next = (index + options.length - 1) % options.length;
      if (event.key === "Home") next = 0;
      if (event.key === "End") next = options.length - 1;
      if (next !== undefined) { event.preventDefault(); options[next].focus(); }
    });
    picker.addEventListener("focusout", event => { if (!picker.contains(event.relatedTarget)) close(); });
    document.addEventListener("pointerdown", event => { if (!picker.contains(event.target)) close(); });
    window.addEventListener("hashchange", () => close());
    window.addEventListener("storage", event => {
      if (event.key === storageKey || event.key === null) { selected = normalize(event.newValue); sync(); }
    });
    const pauseDecorations = () => root.classList.toggle("theme-paused", document.hidden);
    document.addEventListener("visibilitychange", pauseDecorations);
    pauseDecorations();
    sync();
  });
})();

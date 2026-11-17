// The window's title «<page> — <app>»: the app's name is whatever follows the
// last « — » of the title the core set (AddonPage: «<route title> — <app>»),
// so the addon needs no copy of the brand.
export function setTitle(page: string): void {
  const cur = document.title
  const i = cur.lastIndexOf(' — ')
  const app = i >= 0 ? cur.slice(i + 3) : cur
  document.title = app && app !== page ? `${page} — ${app}` : page
}

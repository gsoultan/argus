// Runs before the console's bundle, blocking, so the first paint is already in
// the scheme the person chose. Without it the page renders light and flips to
// dark once React mounts -- a white flash on every load for anyone on dark.
//
// The key and the values must match src/main.tsx (COLOR_SCHEME_KEY in
// src/theme): "light", "dark", or "auto" to follow the operating system.
;(function () {
  var scheme = 'auto'
  try {
    var saved = window.localStorage.getItem('argus-color-scheme')
    if (saved === 'light' || saved === 'dark' || saved === 'auto') scheme = saved
  } catch (e) {
    // Storage blocked (private mode, policy): follow the system.
  }
  if (scheme === 'auto') {
    scheme = window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
  }
  document.documentElement.setAttribute('data-mantine-color-scheme', scheme)
})()

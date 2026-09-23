// The tab icon, at a URL no browser has cached.
//
// Keycloak's login template points the icon at img/favicon.ico and nothing in
// a theme can version that path. Before this theme shipped an icon of its
// own, that URL answered with Keycloak's — cached, like every theme resource,
// for as long as the server's Cache-Control said, which used to be 30 days.
// Every browser that saw the sign-in page inside that window keeps showing
// the old icon however often the file behind the URL changes.
//
// So point the icon somewhere else. The query is ignored by the resource
// handler but makes the URL one no cache holds. Raise the number whenever the
// icon itself changes.
//
// This is the whole of the theme's behaviour: one attribute of one element,
// and the page works the same with the script blocked.
(function () {
  try {
    var here = document.currentScript && document.currentScript.src;
    if (!here) return;
    var icon = document.querySelector("link[rel~='icon']");
    if (!icon) return;
    icon.href = here.replace(/\/js\/brand\.js.*$/, "/img/favicon.ico?v=4");
  } catch (e) {
    /* The stock icon is a fine fallback; never break sign-in over a picture. */
  }
})();

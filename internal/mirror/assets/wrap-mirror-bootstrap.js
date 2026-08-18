(() => {
  const storageKey = "wrap.mirror.v3.secret";
  const fragment = new URLSearchParams(location.hash.slice(1));
  const fragmentKey = fragment.get("k");
  history.replaceState(null, "", location.pathname + location.search);
  if (fragmentKey !== null) {
    try {
      if (!/^[A-Za-z0-9_-]{43}$/.test(fragmentKey)) {
        sessionStorage.removeItem(storageKey);
      } else {
        sessionStorage.setItem(storageKey, fragmentKey);
      }
    } catch {
      // Storage availability is handled by the main client. The credential has
      // already been removed from the address bar and browser history.
    }
  }

  const nativeImporter = () => import("/assets/wrap-mirror.js");
  const importer = typeof globalThis.__wrapMirrorImport === "function"
    ? globalThis.__wrapMirrorImport
    : nativeImporter;
  delete globalThis.__wrapMirrorImport;

  Promise.resolve()
    .then(importer)
    .catch(() => {
      const connectionState = document.getElementById("connection-state");
      const messageView = document.getElementById("message-view");
      const messageTitle = document.getElementById("message-title");
      const messageDetail = document.getElementById("message-detail");

      connectionState.textContent = "Unavailable";
      connectionState.classList.add("failure");
      messageView.classList.add("load-failure");
      messageTitle.textContent = "Remote client failed to load";
      messageDetail.textContent =
        "Required browser asset unavailable. Reload after updating Wrap. " +
        "Code: client_asset_unavailable";
    });
})();

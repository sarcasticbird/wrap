(() => {
  // Reopening a pairing URL in this tab can be a fragment-only navigation.
  // Reload so the client consumes the replacement credential from the fragment.
  globalThis.addEventListener("hashchange", () => {
    if (new URLSearchParams(location.hash.slice(1)).get("k") !== null) {
      location.reload();
    }
  });

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

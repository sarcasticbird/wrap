package mirror

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

func browserBootstrapSource(t *testing.T) string {
	t.Helper()
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror-bootstrap.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	const nativeImport = `() => import("/assets/wrap-mirror.js")`
	if !strings.Contains(source, nativeImport) {
		t.Fatal("bootstrap is missing its native dynamic import")
	}
	// goja does not parse dynamic import syntax. Replace only the unused native
	// branch; the rejecting seam below remains the importer selected by bootstrap.
	return strings.Replace(source, nativeImport, `() => Promise.resolve()`, 1)
}

func installBrowserURLSearchParams(t *testing.T, runtime *goja.Runtime) {
	t.Helper()
	if _, err := runtime.RunString(`
globalThis.URLSearchParams = class {
  constructor(value) {
    this.values = Object.create(null);
    for (const field of String(value).split("&")) {
      if (field === "") continue;
      const separator = field.indexOf("=");
      const rawKey = separator < 0 ? field : field.slice(0, separator);
      const rawValue = separator < 0 ? "" : field.slice(separator + 1);
      this.values[decodeURIComponent(rawKey)] = decodeURIComponent(rawValue.replaceAll("+", " "));
    }
  }
  get(key) {
    return Object.prototype.hasOwnProperty.call(this.values, key) ? this.values[key] : null;
  }
};
`); err != nil {
		t.Fatalf("install browser URLSearchParams: %v", err)
	}
}

func TestBrowserBootstrapShowsSafeFailureWhenClientImportRejects(t *testing.T) {
	source := browserBootstrapSource(t)

	runtime := goja.New()
	installBrowserURLSearchParams(t, runtime)
	if _, err := runtime.RunString(`
const elements = {
  "connection-state": {textContent: "Preparing", classList: {values: [], add(value) { this.values.push(value); }}},
  "message-view": {classList: {values: [], add(value) { this.values.push(value); }}},
  "message-title": {textContent: "Checking encryption…"},
  "message-detail": {textContent: "The terminal stays locked until pairing succeeds."},
};
const credential = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";
globalThis.document = {getElementById(id) { return elements[id]; }};
globalThis.location = {hash: "#k=" + credential, pathname: "/mirror", search: "?source=uat"};
globalThis.__historyCalls = [];
globalThis.history = {replaceState(state, title, url) { __historyCalls.push({state, title, url}); location.hash = ""; }};
globalThis.__storedCredential = null;
globalThis.sessionStorage = {
  setItem(key, value) { if (key === "wrap.mirror.v1.secret") __storedCredential = value; },
  removeItem(key) { if (key === "wrap.mirror.v1.secret") __storedCredential = null; },
};
globalThis.__bootstrapElements = elements;
globalThis.__webSocketCalls = 0;
globalThis.WebSocket = function () { globalThis.__webSocketCalls += 1; };
`); err != nil {
		t.Fatalf("set up bootstrap browser: %v", err)
	}
	promise, _, reject := runtime.NewPromise()
	if err := reject(errors.New("SENTINEL raw import failure https://secret.invalid/#k=credential")); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Set("__wrapMirrorImport", func() *goja.Promise { return promise }); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.RunString(source); err != nil {
		t.Fatalf("run browser bootstrap: %v", err)
	}
	if _, err := runtime.RunString(`
function check(ok, message) {
  if (!ok) throw new Error(message);
}
const rendered = [
  __bootstrapElements["connection-state"].textContent,
  __bootstrapElements["message-title"].textContent,
  __bootstrapElements["message-detail"].textContent,
].join("\n");
check(__bootstrapElements["connection-state"].textContent === "Unavailable", "connection state stayed pending");
check(__bootstrapElements["connection-state"].classList.values.includes("failure"), "connection state lacks failure styling");
check(__bootstrapElements["message-view"].classList.values.includes("load-failure"), "message panel lacks failure styling");
check(__bootstrapElements["message-title"].textContent === "Remote client failed to load", "safe title was not rendered");
check(rendered.includes("Required browser asset unavailable. Reload after updating Wrap."), "safe recovery copy was not rendered");
check(rendered.includes("Code: client_asset_unavailable"), "safe error code was not rendered");
check(!rendered.includes("SENTINEL"), "raw import failure was rendered");
check(!rendered.includes("credential"), "credential-bearing rejection was rendered");
check(__historyCalls.length === 1, "bootstrap did not clear the credential fragment");
check(__historyCalls[0].url === "/mirror?source=uat", "bootstrap retained the credential in browser history");
check(location.hash === "", "credential remained in the address bar");
check(__storedCredential === "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "bootstrap did not transfer the credential");
check(!rendered.includes(__storedCredential), "transferred credential was rendered");
check(__webSocketCalls === 0, "bootstrap constructed a WebSocket after import failure");
check(typeof globalThis.__wrapMirrorImport === "undefined", "bootstrap left its importer seam installed");
`); err != nil {
		t.Fatalf("verify browser bootstrap failure: %v", err)
	}
}

func TestBrowserBootstrapDiscardsMalformedFragmentCredential(t *testing.T) {
	runtime := goja.New()
	installBrowserURLSearchParams(t, runtime)
	if _, err := runtime.RunString(`
globalThis.location = {hash: "#k=too-short", pathname: "/mirror", search: ""};
globalThis.__historyURL = null;
globalThis.history = {replaceState(state, title, url) { __historyURL = url; location.hash = ""; }};
globalThis.__storedCredential = "prior-credential";
globalThis.sessionStorage = {
  setItem(key, value) { if (key === "wrap.mirror.v1.secret") __storedCredential = value; },
  removeItem(key) { if (key === "wrap.mirror.v1.secret") __storedCredential = null; },
};
globalThis.__wrapMirrorImport = () => Promise.resolve();
`); err != nil {
		t.Fatalf("set up malformed credential browser: %v", err)
	}
	if _, err := runtime.RunString(browserBootstrapSource(t)); err != nil {
		t.Fatalf("run browser bootstrap: %v", err)
	}
	if _, err := runtime.RunString(`
if (__historyURL !== "/mirror") throw new Error("malformed fragment was not cleared");
if (location.hash !== "") throw new Error("malformed credential remained in address bar");
if (__storedCredential !== null) throw new Error("malformed credential was retained");
if (typeof globalThis.__wrapMirrorImport !== "undefined") throw new Error("importer seam was retained");
`); err != nil {
		t.Fatalf("verify malformed credential handling: %v", err)
	}
}

package mirror

import (
	"encoding/base64"
	"io/fs"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// Execute the real bootstrap and client credential initialization in document
// order, without loading the terminal or requiring browser crypto in goja.
func TestBrowserRetainedFragmentSurvivesStorageLoss(t *testing.T) {
	source, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	client := string(source)
	decoder := client[strings.Index(client, "function decodeBase64URL("):strings.Index(client, "function fromHex(")]
	initialization := client[strings.Index(client, "const fragment ="):strings.Index(client, "if (secret) {")]
	for _, storage := range []string{"available", "lost", "throws"} {
		t.Run(storage, func(t *testing.T) {
			runtime := goja.New()
			installBrowserURLSearchParams(t, runtime)
			if err := runtime.Set("atob", func(value string) string {
				decoded, err := base64.StdEncoding.DecodeString(value)
				if err != nil {
					panic(runtime.NewGoError(err))
				}
				return string(decoded)
			}); err != nil {
				t.Fatal(err)
			}
			if err := runtime.Set("storageMode", storage); err != nil {
				t.Fatal(err)
			}
			_, err := runtime.RunString(`
const credential = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";
const STORAGE_KEY = "wrap.mirror.v3.secret";
// goja's TypedArray.from requires an object rather than a primitive string.
const nativeFrom = Uint8Array.from;
Uint8Array.from = (value, map) => nativeFrom.call(Uint8Array, typeof value === "string" ? value.split("") : value, map);
let stored = credential;
globalThis.location = {hash: "#k=" + credential, pathname: "/", search: ""};
globalThis.history = {replaceState() { location.hash = ""; }};
let storageCalls = 0;
const storage = {
  setItem(key, value) { storageCalls++; if (storageMode === "throws") throw new Error("storage denied"); stored = value; },
  getItem() { storageCalls++; if (storageMode === "throws") throw new Error("storage denied"); return stored; },
  removeItem() { storageCalls++; if (storageMode === "throws") throw new Error("storage denied"); stored = null; },
};
Object.defineProperty(globalThis, "sessionStorage", {get() {
  if (storageMode === "throws") throw new Error("storage access denied");
  return storage;
}});
globalThis.__wrapMirrorImport = () => Promise.resolve();
`)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runtime.RunString(decoder + `decodeBase64URL(credential);`); err != nil {
				t.Fatal(err)
			}
			for _, fragment := range []string{
				"#k=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", // full-link or QR entry
				"#k=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", // same-tab reload
				"#k=BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA", // replacement URL
				"#k=too-short", "#k=", "", "#help", "#k=" + strings.Repeat("!", 43),
			} {
				if err := runtime.Set("nextFragment", fragment); err != nil {
					t.Fatal(err)
				}
				if _, err := runtime.RunString(`location.hash = nextFragment;
globalThis.__wrapMirrorImport = () => Promise.resolve();
` + browserBootstrapSource(t)); err != nil {
					t.Fatal(err)
				}
				// Model the next document starting with lost tab state, not a claim
				// about what causes Safari's reported lifecycle behavior.
				if _, err := runtime.RunString(`if (storageMode === "lost") stored = null;
(() => {` + initialization + `
const expected = /^#k=[A-Za-z0-9_-]{43}$/.test(nextFragment);
if (expected !== Boolean(secret)) throw new Error("unexpected credential recovery");
if (secret && (secret.length !== 32 || (nextFragment.includes("BBBB") ? secret[0] !== 4 : secret[0] !== 0))) throw new Error("wrong recovered secret");
if (location.hash !== nextFragment) throw new Error("fragment not retained for next reload");
if (storageCalls !== 0) throw new Error("client used storage");
})();`); err != nil {
					t.Fatalf("fragment %q: %v", fragment, err)
				}
			}
		})
	}
}

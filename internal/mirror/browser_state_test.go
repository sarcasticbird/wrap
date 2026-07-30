package mirror

import (
	"io/fs"
	"testing"

	"github.com/dop251/goja"
)

func TestBrowserStateAcceptsDelayedCloseAfterStatusAndRevocation(t *testing.T) {
	source, err := fs.ReadFile(assets, "assets/wrap-mirror-state.js")
	if err != nil {
		t.Fatal(err)
	}
	runtime := goja.New()
	if _, err := runtime.RunString(string(source)); err != nil {
		t.Fatalf("load browser state reducer: %v", err)
	}
	if _, err := runtime.RunString(`
const reducer = globalThis.WrapMirrorCloseState;
const state = reducer.create();
const first = {id: "$7", generation: "generation-a"};
const second = {id: "$8", generation: "generation-a"};
const third = {id: "$9", generation: "generation-a"};
function check(ok, message) {
  if (!ok) {
    throw new Error(message);
  }
}

state.sessions = [first, second, third];
check(reducer.open(state, first), "first session did not open");
check(reducer.beginClose(state), "first session did not begin closing");
check(
  reducer.status(state, [second, third]) === "render",
  "status did not resolve the pending close",
);
check(!reducer.canOpen(state), "status allowed open before delayed acknowledgement");
check(
  reducer.acknowledgeClose(state) === "late",
  "status-delayed acknowledgement was rejected",
);
check(reducer.canOpen(state), "status-delayed acknowledgement left state blocked");
check(reducer.open(state, second), "second session could not open");

check(reducer.beginClose(state), "second session did not begin closing");
check(
  reducer.revoked(state, second) === "ended",
  "revocation did not resolve the pending close",
);
check(!reducer.canOpen(state), "revocation allowed open before delayed acknowledgement");
check(
  reducer.acknowledgeClose(state) === "late",
  "revocation-delayed acknowledgement was rejected",
);
check(reducer.canOpen(state), "revocation-delayed acknowledgement left state blocked");
check(reducer.open(state, third), "connection was unusable after delayed acknowledgements");

let rejectedUnexpected = false;
try {
  reducer.acknowledgeClose(state);
} catch {
  rejectedUnexpected = true;
}
check(rejectedUnexpected, "unsolicited close acknowledgement was accepted");
`); err != nil {
		t.Fatalf("browser state sequence: %v", err)
	}
}

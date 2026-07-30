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
let poisoned = false;
let renders = 0;
let endings = 0;
const effects = {
  render: () => { renders += 1; },
  ended: () => { endings += 1; },
  error: () => {},
};
function check(ok, message) {
  if (!ok) {
    throw new Error(message);
  }
}
function deliver(message) {
  try {
    reducer.receiveMessage(state, message, effects);
  } catch {
    poisoned = true;
  }
}

state.sessions = [first, second, third];
check(reducer.open(state, first), "first session did not open");
check(reducer.beginClose(state), "first session did not begin closing");
deliver({type: "status", sessions: [second, third]});
check(!poisoned, "status poisoned the connection");
check(renders === 1, "status did not render the remaining sessions");
check(!reducer.canOpen(state), "status allowed open before delayed acknowledgement");
deliver({type: "close"});
check(!poisoned, "status-delayed acknowledgement poisoned the connection");
check(reducer.canOpen(state), "status-delayed acknowledgement left state blocked");
check(reducer.open(state, second), "second session could not open");

check(reducer.beginClose(state), "second session did not begin closing");
deliver({type: "revoked", session: second});
check(!poisoned, "revocation poisoned the connection");
check(endings === 1, "revocation did not report the ended terminal");
check(!reducer.canOpen(state), "revocation allowed open before delayed acknowledgement");
deliver({type: "close"});
check(!poisoned, "revocation-delayed acknowledgement poisoned the connection");
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

package mirror

import (
	"io/fs"
	"testing"

	"github.com/dop251/goja"
)

func TestBrowserViewportFitsWideHostGeometryWithoutChangingIt(t *testing.T) {
	source, err := fs.ReadFile(assets, "assets/wrap-mirror-viewport.js")
	if err != nil {
		t.Fatal(err)
	}
	runtime := goja.New()
	if _, err := runtime.RunString(string(source)); err != nil {
		t.Fatalf("load browser viewport reducer: %v", err)
	}
	if _, err := runtime.RunString(`
const viewport = globalThis.WrapMirrorViewportState;
const state = viewport.create();
function check(ok, message) {
  if (!ok) throw new Error(message);
}

viewport.open(state, {columns: 160, rows: 50});
viewport.measure(state, {width: 1200, height: 600}, 360);
viewport.readable(state);
let layout = viewport.layout(state);
check(state.columns === 160 && state.rows === 50, "host geometry changed");
check(state.mode === "manual", "sub-50 Fit did not enter readable mode");
check(layout.scale === 0.5, "wide terminal did not open at 50 percent");
check(layout.width === 600 && layout.height === 300, "readable canvas is wrong");
check(state.columns === 160 && state.rows === 50, "readable open changed host geometry");

viewport.fit(state);
layout = viewport.layout(state);
check(layout.scale === 0.3, "wide geometry did not fit");
check(layout.width === 360 && layout.height === 180, "scaled canvas is wrong");
const scaledCell = viewport.fontSize(state, 14);
check(scaledCell === 4.2, "font/cell scale is wrong");
check(Math.floor((37 * scaledCell) / scaledCell) === 37, "scaled pointer cell changed");

const fittingState = viewport.create();
viewport.open(fittingState, {columns: 80, rows: 24});
viewport.measure(fittingState, {width: 400, height: 200}, 240);
viewport.readable(fittingState);
check(fittingState.mode === "fit", "readable Fit left fit mode");
check(viewport.layout(fittingState).scale === 0.6, "readable Fit changed scale");

viewport.resize(state, 300);
layout = viewport.layout(state);
check(layout.scale === 0.25, "viewport resize did not refit");
check(state.columns === 160 && state.rows === 50, "viewport resize changed host geometry");

viewport.resize(state, 240);
check(viewport.layout(state).scale === 0.2, "Fit below 30 percent changed");
let pinch = viewport.beginPinch(state, {
  distance: 100,
  midpointX: 100,
  midpointY: 80,
  scrollLeft: 0,
  scrollTop: 0,
});
check(viewport.previewPinch(state, pinch, {
  distance: 120,
  midpointX: 100,
  midpointY: 80,
}) === null, "inward-floor pinch enlarged sub-30 Fit");
check(state.mode === "fit" && state.scale === 0.2, "sub-floor pinch left Fit");
let result = viewport.previewPinch(state, pinch, {
  distance: 150,
  midpointX: 100,
  midpointY: 80,
});
check(result.scale === 0.3, "pinch preview did not enter manual floor");
check(state.mode === "fit" && state.scale === 0.2, "pinch preview mutated sub-floor Fit");
check(viewport.previewPinch(state, pinch, {
  distance: 120,
  midpointX: 100,
  midpointY: 80,
}) === null, "reversed pinch retained a stale manual preview");
check(state.mode === "fit" && state.scale === 0.2, "reversed pinch left Fit");

viewport.setScale(state, 0.5);
pinch = viewport.beginPinch(state, {
  distance: 100,
  midpointX: 100,
  midpointY: 80,
  scrollLeft: 200,
  scrollTop: 40,
});
const committedScale = state.scale;
const committedMode = state.mode;
result = viewport.previewPinch(state, pinch, {
  distance: 150,
  midpointX: 110,
  midpointY: 90,
});
check(result.scale === 0.75, "pinch preview scale is not continuous");
check(result.width === 900 && result.height === 450, "pinch preview dimensions are wrong");
check(result.scrollLeft === 340 && result.scrollTop === 90, "pinch midpoint moved");
check(state.scale === committedScale && state.mode === committedMode,
  "pinch preview mutated committed state");
const committed = viewport.commitPinch(state, result);
check(committed.scale === 0.75 && state.scale === 0.75 && state.mode === "manual",
  "pinch commit did not apply preview once");
check(state.columns === 160 && state.rows === 50, "pinch commit changed host geometry");
check(viewport.beginPinch(state, {distance: 0}) === null, "zero distance pinch accepted");
const beforeInvalidCommit = state.scale;
check(viewport.commitPinch(state, {scale: Number.NaN}) === null,
  "invalid pinch commit accepted");
check(state.scale === beforeInvalidCommit, "invalid pinch commit mutated state");

const fixedState = viewport.create();
viewport.open(fixedState, {columns: 160, rows: 50});
viewport.measure(fixedState, {
  width: 1200,
  height: 600,
  fixedWidth: 24,
  fixedHeight: 20,
  fixedLeft: 10,
  fixedTop: 8,
}, 624);
viewport.setScale(fixedState, 0.5);
pinch = viewport.beginPinch(fixedState, {
  distance: 100,
  midpointX: 100,
  midpointY: 80,
  scrollLeft: 200,
  scrollTop: 40,
});
result = viewport.previewPinch(fixedState, pinch, {
  distance: 150,
  midpointX: 110,
  midpointY: 90,
});
check(result.width === 924 && result.height === 470,
  "fixed chrome changed preview dimensions");
check(result.scrollLeft === 335 && result.scrollTop === 86,
  "fixed chrome moved the pinch midpoint");
const fixedCommitted = viewport.commitPinch(fixedState, result);
check(fixedCommitted.width === result.width && fixedCommitted.height === result.height,
  "fixed chrome preview and commit geometry differ");

viewport.setScale(state, 0.5);
let vertical = viewport.panVertical(state, {
  scrollTop: 0,
  scrollHeight: 300,
  clientHeight: 500,
  deltaY: 30,
});
check(vertical.scrollTop === 0 && vertical.lineOffset === -5,
  "short canvas did not hand vertical drag to terminal scrollback");
vertical = viewport.panVertical(state, {
  scrollTop: 100,
  scrollHeight: 900,
  clientHeight: 400,
  deltaY: 30,
});
check(vertical.scrollTop === 70 && vertical.lineOffset === 0,
  "tall canvas did not consume available vertical pan");
vertical = viewport.panVertical(state, {
  scrollTop: 500,
  scrollHeight: 900,
  clientHeight: 400,
  deltaY: -30,
});
check(vertical.scrollTop === 500 && vertical.lineOffset === 5,
  "bottom boundary did not hand upward drag to terminal scrollback");
let tracked = viewport.trackLineOffset(0, 0, 100, -5);
check(tracked.lineOffset === 0 && tracked.viewportY === 0,
  "top scrollback clamp created reversal movement");
tracked = viewport.trackLineOffset(0, 100, 100, 5);
check(tracked.lineOffset === 0 && tracked.viewportY === 100,
  "bottom scrollback clamp created reversal movement");
tracked = viewport.trackLineOffset(0, 10, 100, -5);
check(tracked.lineOffset === -5 && tracked.viewportY === 5,
  "applied scrollback movement did not reverse symmetrically");
tracked = viewport.trackLineOffset(tracked.lineOffset, tracked.viewportY, 100, 5);
check(tracked.lineOffset === 0 && tracked.viewportY === 10,
  "requested scrollback movement did not track synchronously");

viewport.setScale(state, 2.5);
check(viewport.layout(state).scale === 2, "manual scale did not clamp at 200 percent");
viewport.fit(state);
check(state.mode === "fit", "fit did not restore fit mode");
check(viewport.layout(state).scale === 0.2, "fit did not use the current viewport width");

viewport.resize(state, 1);
check(viewport.fontSize(state, 14) > 0, "tiny viewport produced an invalid font size");

viewport.reset(state);
check(viewport.layout(state) === null, "reset retained terminal measurements");
check(state.mode === "fit", "reset did not restore fit mode");
`); err != nil {
		t.Fatalf("browser viewport sequence: %v", err)
	}
}

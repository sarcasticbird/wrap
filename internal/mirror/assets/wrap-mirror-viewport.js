(function installWrapMirrorViewportState(global) {
  const MIN_READABLE_SCALE = 0.5;

  function create() {
    return {
      columns: 0,
      rows: 0,
      baseWidth: 0,
      baseHeight: 0,
      fixedWidth: 0,
      fixedHeight: 0,
      fixedLeft: 0,
      fixedTop: 0,
      viewportWidth: 0,
      mode: "fit",
      scale: 1,
    };
  }

  function open(state, geometry) {
    if (
      !Number.isInteger(geometry?.columns) ||
      geometry.columns < 2 ||
      !Number.isInteger(geometry?.rows) ||
      geometry.rows < 2
    ) {
      throw new Error("invalid terminal geometry");
    }
    state.columns = geometry.columns;
    state.rows = geometry.rows;
    state.baseWidth = 0;
    state.baseHeight = 0;
    state.fixedWidth = 0;
    state.fixedHeight = 0;
    state.fixedLeft = 0;
    state.fixedTop = 0;
    state.viewportWidth = 0;
    state.mode = "fit";
    state.scale = 1;
  }

  function measure(state, surface, viewportWidth) {
    if (
      !Number.isFinite(surface?.width) ||
      surface.width <= 0 ||
      !Number.isFinite(surface?.height) ||
      surface.height <= 0
    ) {
      throw new Error("invalid terminal surface measurement");
    }
    state.baseWidth = surface.width;
    state.baseHeight = surface.height;
    state.fixedWidth = Number.isFinite(surface.fixedWidth)
      ? Math.max(0, surface.fixedWidth)
      : 0;
    state.fixedHeight = Number.isFinite(surface.fixedHeight)
      ? Math.max(0, surface.fixedHeight)
      : 0;
    state.fixedLeft = Number.isFinite(surface.fixedLeft)
      ? Math.max(0, surface.fixedLeft)
      : 0;
    state.fixedTop = Number.isFinite(surface.fixedTop)
      ? Math.max(0, surface.fixedTop)
      : 0;
    resize(state, viewportWidth);
  }

  function resize(state, viewportWidth) {
    if (!Number.isFinite(viewportWidth) || viewportWidth <= 0) {
      throw new Error("invalid terminal viewport width");
    }
    state.viewportWidth = viewportWidth;
    if (state.mode === "fit") {
      state.scale = fitScale(state);
    }
  }

  function fitScale(state) {
    return state.baseWidth > 0
      ? Math.min(
        1,
        Math.max(0.01, (state.viewportWidth - state.fixedWidth) / state.baseWidth),
      )
      : 1;
  }

  function readable(state) {
    const fitted = fitScale(state);
    if (fitted >= MIN_READABLE_SCALE) {
      state.mode = "fit";
      state.scale = fitted;
      return;
    }
    setScale(state, MIN_READABLE_SCALE);
  }

  function finite(...values) {
    return values.every((value) => Number.isFinite(value));
  }

  function finitePositive(value) {
    return Number.isFinite(value) && value > 0;
  }

  function setScale(state, scale) {
    if (!Number.isFinite(scale)) {
      throw new Error("invalid terminal scale");
    }
    state.mode = "manual";
    state.scale = manualScale(scale);
  }

  function manualScale(scale) {
    return Math.min(2, Math.max(0.3, Math.round(scale * 1000) / 1000));
  }

  function beginPinch(state, input) {
    const current = layout(state);
    if (
      !current ||
      !finitePositive(input?.distance) ||
      !finite(input?.midpointX, input?.midpointY, input?.scrollLeft, input?.scrollTop)
    ) {
      return null;
    }
    return Object.freeze({
      distance: input.distance,
      scale: state.scale,
      width: current.width,
      height: current.height,
      midpointX: input.midpointX,
      midpointY: input.midpointY,
      scrollLeft: input.scrollLeft,
      scrollTop: input.scrollTop,
      fixedLeft: state.fixedLeft,
      fixedTop: state.fixedTop,
    });
  }

  function previewPinch(state, snapshot, input) {
    if (
      !snapshot ||
      !finitePositive(snapshot.distance) ||
      !finitePositive(snapshot.scale) ||
      !finitePositive(snapshot.width) ||
      !finitePositive(snapshot.height) ||
      !finite(
        snapshot.midpointX,
        snapshot.midpointY,
        snapshot.scrollLeft,
        snapshot.scrollTop,
        input?.midpointX,
        input?.midpointY,
      ) ||
      !finitePositive(input?.distance)
    ) {
      return null;
    }
    const candidate = snapshot.scale * input.distance / snapshot.distance;
    if (!finitePositive(candidate) || (snapshot.scale < 0.3 && candidate < 0.3)) {
      return null;
    }
    const scale = manualScale(candidate);
    const next = layoutAtScale(state, scale);
    if (!next) {
      return null;
    }
    return {
      scale,
      width: next.width,
      height: next.height,
      scrollLeft: (snapshot.scrollLeft + snapshot.midpointX - snapshot.fixedLeft) *
        scale / snapshot.scale + snapshot.fixedLeft - input.midpointX,
      scrollTop: (snapshot.scrollTop + snapshot.midpointY - snapshot.fixedTop) *
        scale / snapshot.scale + snapshot.fixedTop - input.midpointY,
    };
  }

  function commitPinch(state, preview) {
    if (!finitePositive(preview?.scale)) {
      return null;
    }
    setScale(state, preview.scale);
    return layout(state);
  }

  function panVertical(state, input) {
    if (
      state.rows <= 0 ||
      !finitePositive(state.baseHeight) ||
      !finitePositive(state.scale) ||
      !finite(
        input?.scrollTop,
        input?.scrollHeight,
        input?.clientHeight,
        input?.deltaY,
      ) ||
      input.scrollHeight < 0 ||
      input.clientHeight < 0
    ) {
      return null;
    }
    const maximum = Math.max(0, input.scrollHeight - input.clientHeight);
    const desired = input.scrollTop - input.deltaY;
    const scrollTop = Math.min(maximum, Math.max(0, desired));
    const lineHeight = state.baseHeight * state.scale / state.rows;
    return {
      scrollTop,
      lineOffset: Math.trunc((desired - scrollTop) / lineHeight),
    };
  }

  // xterm applies scrollLines through its DOM viewport asynchronously. Keep an
  // intended target for this gesture instead of rereading a temporarily stale
  // activeBuffer.viewportY after every request.
  function trackLineOffset(currentOffset, targetViewportY, baseY, lineDelta) {
    if (!finite(currentOffset, targetViewportY, baseY, lineDelta) || baseY < 0) {
      return { lineOffset: currentOffset, viewportY: targetViewportY };
    }
    const nextViewportY = Math.min(baseY, Math.max(0, targetViewportY + lineDelta));
    return {
      lineOffset: currentOffset + nextViewportY - targetViewportY,
      viewportY: nextViewportY,
    };
  }

  function fit(state) {
    state.mode = "fit";
    state.scale = fitScale(state);
  }

  function layout(state) {
    return layoutAtScale(state, state.scale);
  }

  function layoutAtScale(state, scale) {
    if (state.baseWidth <= 0 || state.baseHeight <= 0) {
      return null;
    }
    return {
      scale,
      width: Math.ceil(state.baseWidth * scale + state.fixedWidth),
      height: Math.ceil(state.baseHeight * scale + state.fixedHeight),
    };
  }

  function fontSize(state, baseFontSize) {
    if (!Number.isFinite(baseFontSize) || baseFontSize <= 0) {
      throw new Error("invalid base terminal font size");
    }
    return baseFontSize * state.scale;
  }

  function reset(state) {
    Object.assign(state, create());
  }

  global.WrapMirrorViewportState = Object.freeze({
    create,
    open,
    measure,
    resize,
    layout,
    fontSize,
    fit,
    reset,
    setScale,
    readable,
    beginPinch,
    previewPinch,
    commitPinch,
    panVertical,
    trackLineOffset,
  });
})(globalThis);

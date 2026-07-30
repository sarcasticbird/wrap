(function installWrapMirrorCloseState(global) {
  function sameSession(left, right) {
    return Boolean(left && right &&
      left.id === right.id &&
      left.generation === right.generation);
  }

  function create() {
    return {
      sessions: [],
      current: null,
      closing: null,
      awaitingCloseAcknowledgement: false,
    };
  }

  function canOpen(state) {
    return !state.current &&
      !state.closing &&
      !state.awaitingCloseAcknowledgement;
  }

  function open(state, session) {
    if (!canOpen(state)) {
      return false;
    }
    state.current = session;
    return true;
  }

  function beginClose(state) {
    if (!state.current || state.closing || state.awaitingCloseAcknowledgement) {
      return false;
    }
    state.closing = state.current;
    state.current = null;
    return true;
  }

  function status(state, nextSessions) {
    state.sessions = nextSessions;
    if (state.closing) {
      const closingStillMirrored = nextSessions.some(
        (session) => sameSession(session, state.closing),
      );
      if (closingStillMirrored) {
        return "none";
      }
      state.awaitingCloseAcknowledgement = true;
      state.closing = null;
      return "render";
    }
    if (state.current) {
      const currentStillMirrored = nextSessions.some(
        (session) => sameSession(session, state.current),
      );
      if (!currentStillMirrored) {
        state.current = null;
        return "ended";
      }
      return "none";
    }
    return "render";
  }

  function acknowledgeClose(state) {
    if (state.awaitingCloseAcknowledgement) {
      state.awaitingCloseAcknowledgement = false;
      return "late";
    }
    if (state.closing) {
      state.closing = null;
      return "render";
    }
    throw new Error("unexpected close acknowledgement");
  }

  function revoked(state, session) {
    const isCurrent = sameSession(state.current, session);
    const isClosing = sameSession(state.closing, session);
    state.sessions = state.sessions.filter((candidate) => !sameSession(candidate, session));
    if (isCurrent) {
      state.current = null;
    }
    if (isClosing) {
      state.awaitingCloseAcknowledgement = true;
      state.closing = null;
    }
    return isCurrent || isClosing ? "ended" : "none";
  }

  function error(state) {
    if (state.closing) {
      state.awaitingCloseAcknowledgement = true;
    }
    state.current = null;
    state.closing = null;
    return "error";
  }

  function reset(state) {
    state.current = null;
    state.closing = null;
    state.awaitingCloseAcknowledgement = false;
  }

  global.WrapMirrorCloseState = Object.freeze({
    create,
    canOpen,
    open,
    beginClose,
    status,
    acknowledgeClose,
    revoked,
    error,
    reset,
  });
})(globalThis);

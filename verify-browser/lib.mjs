// lib.mjs is the pure half of the verify-browser script: input normalisation,
// the allowed-host check, first-party filtering, the caps and the verdict.
// It must not import Playwright — `node --test` runs it without a browser.
// The contract it implements is docs/VERIFY_BROWSER.md.

export const SCHEMA_VERSION = 1;

export const LIMITS = Object.freeze({
  maxSteps: 30,
  maxAssertions: 30,
  maxStringChars: 2000,
  minTimeoutMs: 5000,
  maxTimeoutMs: 90000,
  stepTimeoutMs: 10000,
});

export const CAPS = Object.freeze({
  pageErrors: 10,
  pageErrorChars: 500,
  consoleErrors: 20,
  consoleErrorChars: 300,
  failedRequests: 20,
  blockedNavigations: 10,
  detailChars: 300,
  urlChars: 2000,
  domTextChars: 1000,
});

export const ACTIONS = Object.freeze(['goto', 'click', 'fill', 'press', 'wait_for']);
export const ASSERTION_TYPES = Object.freeze(['text_visible', 'text_absent', 'selector_visible', 'url_path']);

// normalizeHost lowercases a host and strips one trailing dot, the way the
// runner's preview record and agentbox's proxy compare hosts.
export function normalizeHost(host) {
  return String(host ?? '').trim().toLowerCase().replace(/\.$/, '');
}

// isAllowedUrl is the allowed-host check: https only, no user info, port 443
// or none, and the host exactly equal to one of allowedHosts.
export function isAllowedUrl(raw, allowedHosts) {
  let u;
  try {
    u = new URL(String(raw));
  } catch {
    return false;
  }
  if (u.protocol !== 'https:' || u.username !== '' || u.password !== '') {
    return false;
  }
  // WHATWG URL drops the default port, so "" covers both :443 and none.
  if (u.port !== '') {
    return false;
  }
  const host = normalizeHost(u.hostname);
  if (host === '') {
    return false;
  }
  return allowedHosts.some((h) => normalizeHost(h) === host);
}

// hostOf returns the normalised host of a URL, or "" when it doesn't parse.
export function hostOf(raw) {
  try {
    return normalizeHost(new URL(String(raw)).hostname);
  } catch {
    return '';
  }
}

function charCount(s) {
  return [...s].length;
}

function requireString(obj, key, where, { allowEmpty = false } = {}) {
  const v = obj[key];
  if (typeof v !== 'string') {
    throw new Error(`${where}: ${key} must be a string`);
  }
  if (!allowEmpty && v.trim() === '') {
    throw new Error(`${where}: ${key} is required`);
  }
  if (charCount(v) > LIMITS.maxStringChars) {
    throw new Error(`${where}: ${key} is longer than ${LIMITS.maxStringChars} characters`);
  }
  return v;
}

// checkStringFields applies the per-string limit to every string field of a
// step or assertion, including ones its action or type doesn't use.
function checkStringFields(obj, where) {
  for (const [key, v] of Object.entries(obj)) {
    if (typeof v === 'string' && charCount(v) > LIMITS.maxStringChars) {
      throw new Error(`${where}: ${key} is longer than ${LIMITS.maxStringChars} characters`);
    }
  }
}

function normalizeStep(step, i, allowedHosts) {
  const where = `steps[${i}]`;
  if (step === null || typeof step !== 'object') {
    throw new Error(`${where}: must be an object`);
  }
  checkStringFields(step, where);
  switch (step.action) {
    case 'goto': {
      const url = requireString(step, 'url', where);
      if (!isAllowedUrl(url, allowedHosts)) {
        throw new Error(`${where}: url must be https on a host in allowed_hosts`);
      }
      return { action: 'goto', url };
    }
    case 'click':
      return { action: 'click', selector: requireString(step, 'selector', where) };
    case 'fill':
      return {
        action: 'fill',
        selector: requireString(step, 'selector', where),
        value: requireString(step, 'value', where, { allowEmpty: true }),
      };
    case 'press':
      return { action: 'press', selector: requireString(step, 'selector', where), key: requireString(step, 'key', where) };
    case 'wait_for': {
      const hasSelector = step.selector !== undefined && step.selector !== '';
      const hasText = step.text !== undefined && step.text !== '';
      if (hasSelector === hasText) {
        throw new Error(`${where}: wait_for takes exactly one of selector or text`);
      }
      return hasSelector
        ? { action: 'wait_for', selector: requireString(step, 'selector', where) }
        : { action: 'wait_for', text: requireString(step, 'text', where) };
    }
    default:
      throw new Error(`${where}: unknown action ${JSON.stringify(step.action)}`);
  }
}

function normalizeAssertion(a, i) {
  const where = `assertions[${i}]`;
  if (a === null || typeof a !== 'object') {
    throw new Error(`${where}: must be an object`);
  }
  checkStringFields(a, where);
  switch (a.type) {
    case 'text_visible':
    case 'text_absent':
      return { type: a.type, text: requireString(a, 'text', where) };
    case 'selector_visible':
      return { type: a.type, selector: requireString(a, 'selector', where) };
    case 'url_path': {
      const path = requireString(a, 'path', where);
      if (!path.startsWith('/')) {
        throw new Error(`${where}: path must start with /`);
      }
      return { type: a.type, path };
    }
    default:
      throw new Error(`${where}: unknown type ${JSON.stringify(a.type)}`);
  }
}

// normalizeInput validates input.json (the Go entrypoint has already checked
// it; this is the script's own guard) and returns a clean copy. It throws an
// Error whose message says what is wrong.
export function normalizeInput(raw) {
  if (raw === null || typeof raw !== 'object') {
    throw new Error('input must be a JSON object');
  }
  if (raw.schema_version !== SCHEMA_VERSION) {
    throw new Error(`schema_version must be ${SCHEMA_VERSION}`);
  }
  if (!Array.isArray(raw.allowed_hosts) || raw.allowed_hosts.length === 0) {
    throw new Error('allowed_hosts must be a non-empty array');
  }
  const allowedHosts = raw.allowed_hosts.map((h, i) => {
    if (typeof h !== 'string' || normalizeHost(h) === '') {
      throw new Error(`allowed_hosts[${i}] must be a host name`);
    }
    return normalizeHost(h);
  });
  const url = requireString(raw, 'url', 'input');
  if (!isAllowedUrl(url, allowedHosts)) {
    throw new Error('url must be https on a host in allowed_hosts');
  }
  const steps = raw.steps ?? [];
  const assertions = raw.assertions ?? [];
  if (!Array.isArray(steps) || steps.length > LIMITS.maxSteps) {
    throw new Error(`steps must be an array of at most ${LIMITS.maxSteps}`);
  }
  if (!Array.isArray(assertions) || assertions.length > LIMITS.maxAssertions) {
    throw new Error(`assertions must be an array of at most ${LIMITS.maxAssertions}`);
  }
  const t = raw.timeout_ms;
  if (!Number.isInteger(t) || t < LIMITS.minTimeoutMs || t > LIMITS.maxTimeoutMs) {
    throw new Error(`timeout_ms must be an integer between ${LIMITS.minTimeoutMs} and ${LIMITS.maxTimeoutMs}`);
  }
  return {
    url,
    allowedHosts,
    steps: steps.map((s, i) => normalizeStep(s, i, allowedHosts)),
    assertions: assertions.map((a, i) => normalizeAssertion(a, i)),
    timeoutMs: t,
  };
}

// capString truncates s to max characters (code points), marking the cut.
export function capString(s, max) {
  const str = String(s ?? '');
  const chars = [...str];
  if (chars.length <= max) {
    return str;
  }
  return chars.slice(0, max - 1).join('') + '…';
}

// oneLine collapses whitespace and strips ANSI colour codes, for Playwright
// error messages that carry a multi-line call log.
export function oneLine(s) {
  // eslint-disable-next-line no-control-regex
  return String(s ?? '').replace(/\u001b\[[0-9;]*m/g, '').replace(/\s+/g, ' ').trim();
}

// isNoiseFailure reports failures that say nothing about the page: requests
// the browser itself cancelled because the page navigated away. Only an
// abort that a navigation explains counts — the page cancelling its own
// request (an AbortController on a stalled API call) is still a failure.
export function isNoiseFailure(errorText, abortedByNavigation) {
  return errorText === 'net::ERR_ABORTED' && abortedByNavigation === true;
}

// abortedByNavigation reports whether a navigation explains a cancelled
// request: a main-frame navigation began after it was sent (the page
// unloading), or — for a navigation request — a later navigation in its own
// frame superseded it. sent and now are {main, frame} navigation counts: when
// the request was sent (its own navigation included) and when it failed.
export function abortedByNavigation({ isNavigation, sent, now }) {
  if (now.main > sent.main) {
    return true;
  }
  return isNavigation === true && now.frame > sent.frame;
}

// blameBlockedNavigation fails the steps that may have scheduled a navigation
// blocked after they were recorded (a timer after a click, say). Timers die
// with their document, so the candidates are the steps that ran while the
// document the block happened in (doc, a count of main-frame document loads)
// was current; when none did, the last step is blamed. Each step has
// {ok, error, startDoc, endDoc}. It returns the number of steps it failed.
export function blameBlockedNavigation(steps, doc, url) {
  if (steps.length === 0) {
    return 0;
  }
  let candidates = steps.filter((s) => s.startDoc <= doc && s.endDoc >= doc);
  if (candidates.length === 0) {
    candidates = [steps[steps.length - 1]];
  }
  let n = 0;
  for (const s of candidates) {
    if (s.ok) {
      s.ok = false;
      s.error =
        `navigation to ${url} was blocked after this step: it is outside this Task's previews ` +
        '(scheduled by this step or another step on the same page)';
      n++;
    }
  }
  return n;
}

// firstPartyFailure returns the failed_requests entry for a request, or null
// when it must not be reported: a request to a host outside allowedHosts
// (third-party noise doesn't fail a check), a response below 400, or a
// cancellation caused by navigation (abortedByNavigation, which the caller
// works out from the main frame's navigations).
export function firstPartyFailure({ url, method, status, failure, abortedByNavigation = false }, allowedHosts) {
  const host = hostOf(url);
  if (host === '' || !allowedHosts.includes(host)) {
    return null;
  }
  const st = Number.isInteger(status) ? status : 0;
  const why = failure ? String(failure) : '';
  if (why !== '') {
    if (isNoiseFailure(why, abortedByNavigation)) {
      return null;
    }
  } else if (st < 400) {
    return null;
  }
  return {
    url: capString(url, CAPS.urlChars),
    method: String(method ?? 'GET'),
    status: st,
    failure: capString(why, CAPS.detailChars),
  };
}

// pushCapped appends value to list unless the list already holds max
// entries or an identical entry.
export function pushCapped(list, value, max) {
  if (list.length >= max || list.includes(value)) {
    return;
  }
  list.push(value);
}

// computeVerdict is the contract's rule: "pass" only when the first load's
// final response is 2xx, every step succeeded, every assertion passed, and
// there are no page errors and no failed first-party requests. Console
// errors, timings and blocked navigations on their own never decide it (a
// blocked navigation fails the step that caused it).
export function computeVerdict(r) {
  const ok =
    Number.isInteger(r.http_status) &&
    r.http_status >= 200 &&
    r.http_status < 300 &&
    r.steps.every((s) => s.ok) &&
    r.assertions.every((a) => a.passed) &&
    r.page_errors.length === 0 &&
    r.failed_requests.length === 0;
  return ok ? 'pass' : 'fail';
}

function roundTiming(v) {
  return typeof v === 'number' && Number.isFinite(v) && v >= 0 ? Math.round(v) : undefined;
}

// buildResult assembles a completed result.json from what the browser
// collected, applying every cap and the verdict.
export function buildResult(c) {
  const result = {
    schema_version: SCHEMA_VERSION,
    status: 'completed',
    verdict: 'fail',
    http_status: Number.isInteger(c.httpStatus) ? c.httpStatus : 0,
    final_url: capString(c.finalUrl ?? '', CAPS.urlChars),
    steps: (c.steps ?? []).map((s, i) => ({
      index: i,
      action: s.action,
      ok: !!s.ok,
      error: s.ok ? '' : capString(oneLine(s.error), CAPS.detailChars),
    })),
    assertions: (c.assertions ?? []).map((a, i) => ({
      index: i,
      type: a.type,
      passed: !!a.passed,
      detail: capString(oneLine(a.detail), CAPS.detailChars),
    })),
    page_errors: (c.pageErrors ?? []).slice(0, CAPS.pageErrors).map((e) => capString(e, CAPS.pageErrorChars)),
    console_errors: (c.consoleErrors ?? []).slice(0, CAPS.consoleErrors).map((e) => capString(e, CAPS.consoleErrorChars)),
    failed_requests: (c.failedRequests ?? []).slice(0, CAPS.failedRequests),
    blocked_navigations: (c.blockedNavigations ?? [])
      .slice(0, CAPS.blockedNavigations)
      .map((u) => capString(u, CAPS.urlChars)),
    timings_ms: {},
    dom_text_snippet: capString(String(c.domText ?? '').trim(), CAPS.domTextChars),
  };
  const t = c.timings ?? {};
  for (const [key, value] of [
    ['dom_content_loaded', t.domContentLoaded],
    ['load', t.load],
    ['lcp', t.lcp],
  ]) {
    const v = roundTiming(value);
    if (v !== undefined) {
      result.timings_ms[key] = v;
    }
  }
  result.verdict = computeVerdict(result);
  return result;
}

// errorResult is result.json for a check that could not run.
export function errorResult(message) {
  return {
    schema_version: SCHEMA_VERSION,
    status: 'error',
    error: capString(oneLine(message), CAPS.detailChars * 3),
  };
}

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
  blockedRequests: 20,
  blockedNavigations: 10,
  detailChars: 300,
  urlChars: 2000,
  domTextChars: 1000,
  outlineEntries: 60,
  outlineTextChars: 120,
  refusedCookies: 20,
});

// VIEWPORT_LIMITS bound a custom {width, height} viewport.
export const VIEWPORT_LIMITS = Object.freeze({
  minWidth: 320,
  maxWidth: 2560,
  minHeight: 480,
  maxHeight: 1600,
});

// VIEWPORT_PRESETS are the named window sizes. tablet and mobile are
// playwright-core's device descriptors (device), whose viewport must be the
// size given here; desktop is Playwright's default context, no emulation.
export const VIEWPORT_PRESETS = Object.freeze({
  desktop: Object.freeze({ width: 1280, height: 720, mobile: false, device: '' }),
  tablet: Object.freeze({ width: 712, height: 1138, mobile: true, device: 'Galaxy Tab S4' }),
  mobile: Object.freeze({ width: 412, height: 839, mobile: true, device: 'Pixel 7' }),
});

export const ACTIONS = Object.freeze(['goto', 'click', 'fill', 'press', 'wait_for', 'back']);
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
    case 'back':
      return { action: 'back' };
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

// normalizeViewport validates input.json's optional viewport: absent is the
// desktop preset; else exactly {preset} or {width, height}. It returns
// {preset, width, height, mobile, device}, preset and device '' for a custom
// size.
export function normalizeViewport(raw) {
  if (raw === undefined) {
    return { preset: 'desktop', ...VIEWPORT_PRESETS.desktop };
  }
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) {
    throw new Error('viewport must be an object: {"preset": ...} or {"width": ..., "height": ...}');
  }
  const keys = Object.keys(raw).sort().join(',');
  if (keys === 'preset') {
    const p = raw.preset;
    if (typeof p !== 'string' || !Object.hasOwn(VIEWPORT_PRESETS, p)) {
      throw new Error(`viewport.preset must be desktop, tablet or mobile, got ${JSON.stringify(p)}`);
    }
    return { preset: p, ...VIEWPORT_PRESETS[p] };
  }
  if (keys === 'height,width') {
    const L = VIEWPORT_LIMITS;
    for (const [key, min, max] of [
      ['width', L.minWidth, L.maxWidth],
      ['height', L.minHeight, L.maxHeight],
    ]) {
      const v = raw[key];
      if (!Number.isInteger(v) || v < min || v > max) {
        throw new Error(`viewport.${key} must be an integer between ${min} and ${max}, got ${JSON.stringify(v)}`);
      }
    }
    return { preset: '', width: raw.width, height: raw.height, mobile: false, device: '' };
  }
  if (Object.hasOwn(raw, 'preset') && (Object.hasOwn(raw, 'width') || Object.hasOwn(raw, 'height'))) {
    throw new Error('viewport takes either preset or width and height, not both');
  }
  throw new Error('viewport must be {"preset": ...} or {"width": ..., "height": ...}');
}

// viewportContextOptions returns the browser context options for a
// normalised viewport. devices is playwright-core's descriptor map; a preset
// uses its descriptor's context options (not defaultBrowserType), and throws
// when the descriptor's viewport is not the documented size.
export function viewportContextOptions(viewport, devices) {
  if (viewport.device === '') {
    return {
      viewport: { width: viewport.width, height: viewport.height },
      deviceScaleFactor: 1,
      isMobile: false,
      hasTouch: false,
    };
  }
  const d = devices?.[viewport.device];
  if (!d) {
    throw new Error(`playwright-core has no "${viewport.device}" device descriptor`);
  }
  if (d.viewport?.width !== viewport.width || d.viewport?.height !== viewport.height) {
    throw new Error(
      `playwright-core's "${viewport.device}" viewport is ${d.viewport?.width}×${d.viewport?.height}, ` +
        `not ${viewport.width}×${viewport.height}`,
    );
  }
  // eslint-disable-next-line no-unused-vars
  const { defaultBrowserType, ...options } = d;
  return options;
}

// resultViewport is the result's viewport: the size used, the preset when
// there was one, and whether the browser emulated a mobile device.
export function resultViewport(viewport) {
  const v = viewport ?? normalizeViewport(undefined);
  const out = { width: v.width, height: v.height };
  if (v.preset) {
    out.preset = v.preset;
  }
  out.mobile = v.mobile === true;
  return out;
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
  const readOnly = normalizeReadOnly(raw.read_only);
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
    viewport: normalizeViewport(raw.viewport),
    readOnly,
    signIn: normalizeSignIn(raw.sign_in, allowedHosts, readOnly),
    screenshot: normalizeScreenshot(raw.screenshot),
  };
}

// normalizeReadOnly validates input.json's optional read_only: absent is
// false; anything but true or false is an error.
export function normalizeReadOnly(raw) {
  return optionalBoolean('read_only', raw);
}

// normalizeScreenshot validates input.json's optional screenshot as
// normalizeReadOnly does read_only.
export function normalizeScreenshot(raw) {
  return optionalBoolean('screenshot', raw);
}

function optionalBoolean(name, raw) {
  if (raw === undefined) {
    return false;
  }
  if (typeof raw !== 'boolean') {
    throw new Error(`${name} must be true or false, got ${JSON.stringify(raw)}`);
  }
  return raw;
}

// SCREENSHOT_ERRORS are the result's screenshot_error values the script sets
// (cmd/verify-browser may also set "too large").
export const SCREENSHOT_ERRORS = Object.freeze({
  noTime: 'not enough time left',
  captureFailed: 'the page could not be captured',
});

// jpegSize returns a JPEG image's {width, height} in pixels, read from its
// first start-of-frame marker, or null when bytes is not a JPEG it can read.
export function jpegSize(bytes) {
  if (!bytes || bytes.length < 4 || bytes[0] !== 0xff || bytes[1] !== 0xd8) {
    return null;
  }
  let i = 2;
  while (i + 4 <= bytes.length) {
    if (bytes[i] !== 0xff) {
      return null;
    }
    const marker = bytes[i + 1];
    if (marker === 0xff) {
      // A fill byte before the marker.
      i++;
      continue;
    }
    if (marker === 0x01 || (marker >= 0xd0 && marker <= 0xd8)) {
      // Markers without a length.
      i += 2;
      continue;
    }
    if (marker === 0xd9 || marker === 0xda) {
      // End of image, or the scan: no frame header before them.
      return null;
    }
    const length = (bytes[i + 2] << 8) | bytes[i + 3];
    if (length < 2) {
      return null;
    }
    // SOF0 to SOF15, except DHT (C4), JPG (C8) and DAC (CC).
    if (marker >= 0xc0 && marker <= 0xcf && marker !== 0xc4 && marker !== 0xc8 && marker !== 0xcc) {
      if (i + 9 > bytes.length) {
        return null;
      }
      const height = (bytes[i + 5] << 8) | bytes[i + 6];
      const width = (bytes[i + 7] << 8) | bytes[i + 8];
      return width > 0 && height > 0 ? { width, height } : null;
    }
    i += 2 + length;
  }
  return null;
}

// SIGN_IN_LIMITS bound sign_in's email and password, in characters.
export const SIGN_IN_LIMITS = Object.freeze({ emailChars: 320, passwordChars: 200 });

// SIGN_IN_REASONS are the result's sign_in.reason values; loadFailed is
// followed by the load error.
export const SIGN_IN_REASONS = Object.freeze({
  noPasswordField: 'the sign-in page has no password field',
  noEmailField: 'the sign-in page has no email field',
  stillOnForm: 'still on the sign-in form 10 seconds after submitting',
  loadFailed: 'the sign-in page could not be loaded: ',
});

// SIGN_IN_NOT_RUN is every step's error and every assertion's detail when
// the sign-in failed.
export const SIGN_IN_NOT_RUN = 'not run: the sign-in failed';

const SIGN_IN_KEYS = Object.freeze(['url', 'email', 'password']);
const LINE_BREAK = /[\n\r\v\f\u0085\u2028\u2029]/;

// normalizeSignIn validates input.json's optional sign_in: absent is null;
// else exactly {url, email, password}, url as the check's url. Its errors
// name the field, never its value.
export function normalizeSignIn(raw, allowedHosts, readOnly) {
  if (raw === undefined) {
    return null;
  }
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) {
    throw new Error('sign_in must be an object: {"url": ..., "email": ..., "password": ...}');
  }
  if (Object.keys(raw).some((k) => !SIGN_IN_KEYS.includes(k))) {
    throw new Error('sign_in takes only url, email and password');
  }
  const url = requireString(raw, 'url', 'sign_in');
  if (!isAllowedUrl(url, allowedHosts)) {
    throw new Error('sign_in: url must be https on a host in allowed_hosts');
  }
  const email = credential(raw, 'email', SIGN_IN_LIMITS.emailChars);
  const password = credential(raw, 'password', SIGN_IN_LIMITS.passwordChars);
  if (readOnly === true) {
    throw new Error('sign_in cannot be used with read_only: a read-only check is of a live site, which has no test users');
  }
  return { url, email, password };
}

function credential(raw, key, max) {
  const v = raw[key];
  if (typeof v !== 'string' || v === '') {
    throw new Error(`sign_in.${key} must be a non-empty string`);
  }
  if (charCount(v) > max) {
    throw new Error(`sign_in.${key} is longer than ${max} characters`);
  }
  if (LINE_BREAK.test(v)) {
    throw new Error(`sign_in.${key} must not contain a line break`);
  }
  return v;
}

// TEXT_LIKE_TYPES are the input types an email field without type="email"
// may have ('' is no type attribute).
const TEXT_LIKE_TYPES = new Set(['', 'text', 'email']);

function inputType(f) {
  return String(f?.type ?? '').trim().toLowerCase();
}

function looksLikeUsername(f) {
  const autocomplete = String(f.autocomplete ?? '').toLowerCase();
  if (autocomplete.includes('username') || autocomplete.includes('email')) {
    return true;
  }
  return [f.name, f.id].some((v) => /email|user|login/i.test(String(v ?? '')));
}

// pickSignInFields chooses the sign-in form's fields among the main frame's
// inputs, described in document order as {type, autocomplete, name, id,
// visible, enabled}: the password field is the first visible, enabled input
// with type="password"; the email field the first with type="email", else
// the first text-like one whose autocomplete, name or id says it holds a user
// name or email. It returns their indexes, -1 for none.
export function pickSignInFields(fields) {
  const list = Array.isArray(fields) ? fields : [];
  const usable = list.flatMap((f, i) => (f && f.visible === true && f.enabled === true ? [i] : []));
  const first = (pred) => usable.find((i) => pred(list[i])) ?? -1;
  const password = first((f) => inputType(f) === 'password');
  let email = first((f) => inputType(f) === 'email');
  if (email === -1) {
    email = first((f) => TEXT_LIKE_TYPES.has(inputType(f)) && looksLikeUsername(f));
  }
  return { password, email };
}

// hasVisiblePasswordField reports whether any described input is a visible
// type="password" one, enabled or not: the sign-in form is still showing.
export function hasVisiblePasswordField(fields) {
  return (Array.isArray(fields) ? fields : []).some((f) => f && f.visible === true && inputType(f) === 'password');
}

// signInPlan says what to do with the fields pickSignInFields chose: 'both'
// (fill email and password, then submit), 'email-first' (a form that asks
// for the email first), or 'fail' with the reason.
export function signInPlan({ password, email }) {
  if (password >= 0) {
    return email >= 0 ? { plan: 'both' } : { plan: 'fail', reason: SIGN_IN_REASONS.noEmailField };
  }
  return email >= 0 ? { plan: 'email-first' } : { plan: 'fail', reason: SIGN_IN_REASONS.noPasswordField };
}

// staysAfterSignIn is whether the check continues on the page the app showed
// after signing in instead of opening its url: url is the site's front page
// (path "/", no query) on the host the browser is on (currentUrl). An app
// that keeps its session in memory would sign out on a reload.
export function staysAfterSignIn(url, currentUrl) {
  let u;
  try {
    u = new URL(String(url));
  } catch {
    return false;
  }
  if (u.pathname !== '/' || u.search !== '') {
    return false;
  }
  const here = hostOf(currentUrl);
  return here !== '' && here === normalizeHost(u.hostname);
}

// cookieName is a refused cookie's name: the parsed cookie's, else the text
// before the first "=" of its Set-Cookie line. A line without "=" is a
// cookie with an empty name whose value is the line, so it gives "".
function cookieName(cookie, cookieLine) {
  if (typeof cookie?.name === 'string') {
    return cookie.name;
  }
  const line = String(cookieLine ?? '');
  const eq = line.indexOf('=');
  return eq < 0 ? '' : line.slice(0, eq).trim();
}

// cookieRefusals lists the cookies a DevTools Network event says the browser
// refused — responseReceivedExtraInfo's blockedCookies (not stored) or
// requestWillBeSentExtraInfo's associatedCookies with blocked reasons (not
// sent) — as {name, reason}, reason being Chromium's first blocked reason.
// Never a value.
export function cookieRefusals(event) {
  const out = [];
  const add = (cookie, cookieLine, reasons) => {
    if (Array.isArray(reasons) && reasons.length > 0) {
      out.push({ name: cookieName(cookie, cookieLine), reason: String(reasons[0]) });
    }
  };
  for (const b of Array.isArray(event?.blockedCookies) ? event.blockedCookies : []) {
    add(b?.cookie, b?.cookieLine, b?.blockedReasons);
  }
  for (const a of Array.isArray(event?.associatedCookies) ? event.associatedCookies : []) {
    add(a?.cookie, undefined, a?.blockedReasons);
  }
  return out;
}

// PENDING_REQUESTS_MAX bounds the refusals RefusedCookies holds for request
// hops it can't attribute yet.
const PENDING_REQUESTS_MAX = 200;

// RefusedCookies collects sign_in.refused_cookies from DevTools Network
// events: request(requestId, url, redirectHasExtraInfo) for each
// requestWillBeSent (a redirect hop reuses the requestId and passes its
// redirectHasExtraInfo; undefined otherwise), response(requestId,
// hasExtraInfo) for each responseReceived, and refused(requestId, kind,
// event) for each ExtraInfo event — kind "request" for
// requestWillBeSentExtraInfo, "response" for responseReceivedExtraInfo.
// Chromium sends ExtraInfo events in order but independently of the hops,
// and only for a hop whose response says it has extra info (a hop served
// from a cache has none), one of each kind. So, as Playwright correlates
// them, the nth event of a kind belongs to the nth hop with extra info; an
// event waits until the hops before it are known, and flush() attributes
// what still waits at the end of the check, taking a hop whose response
// never came as one with extra info. entries holds {host, name, reason} for
// hosts in allowedHosts, deduplicated, at most CAPS.refusedCookies.
export class RefusedCookies {
  constructor(allowedHosts) {
    this.allowedHosts = allowedHosts.map(normalizeHost);
    this.entries = [];
    this.requests = new Map();
    this.pendingCount = 0;
  }

  state(requestId) {
    let st = this.requests.get(requestId);
    if (!st) {
      // hops: {url, extra} with extra true, false or undefined (no response
      // yet); next: per kind, the hop the next event is matched from;
      // queue: per kind, the events (as refusals) not attributed yet.
      st = { hops: [], next: { request: 0, response: 0 }, queue: { request: [], response: [] } };
      this.requests.set(requestId, st);
    }
    return st;
  }

  request(requestId, url, redirectHasExtraInfo) {
    const st = this.state(requestId);
    const last = st.hops[st.hops.length - 1];
    if (last && last.extra === undefined) {
      last.extra = redirectHasExtraInfo !== false;
    }
    st.hops.push({ url: String(url ?? ''), extra: undefined });
    this.match(st, false);
  }

  response(requestId, hasExtraInfo) {
    const st = this.requests.get(requestId);
    const last = st?.hops[st.hops.length - 1];
    if (last && last.extra === undefined) {
      last.extra = hasExtraInfo !== false;
      this.match(st, false);
    }
  }

  refused(requestId, kind, event) {
    const st = this.state(requestId);
    if (this.pendingCount >= PENDING_REQUESTS_MAX) {
      // Still counted, so later events keep their place.
      st.queue[kind].push([]);
    } else {
      const refusals = cookieRefusals(event);
      this.pendingCount += refusals.length;
      st.queue[kind].push(refusals);
    }
    this.match(st, false);
  }

  // flush attributes the events still waiting, at the end of the check.
  flush() {
    for (const st of this.requests.values()) {
      this.match(st, true);
    }
  }

  // match attributes st's queued events to their hops, in order, while the
  // hop of the next one is known (or, final, can be assumed).
  match(st, final) {
    for (const kind of ['request', 'response']) {
      const queue = st.queue[kind];
      while (queue.length > 0) {
        let i = st.next[kind];
        while (i < st.hops.length && st.hops[i].extra === false) {
          i++;
        }
        st.next[kind] = i;
        if (i >= st.hops.length || (st.hops[i].extra === undefined && !final)) {
          break;
        }
        const refusals = queue.shift();
        this.pendingCount -= refusals.length;
        refusals.forEach((r) => this.add(st.hops[i].url, r));
        st.next[kind] = i + 1;
      }
    }
  }

  add(url, { name, reason }) {
    const host = hostOf(url);
    if (host === '' || !this.allowedHosts.includes(host) || this.entries.length >= CAPS.refusedCookies) {
      return;
    }
    const entry = { host, name: redactText(name), reason: capString(reason, CAPS.detailChars) };
    if (!this.entries.some((e) => e.host === entry.host && e.name === entry.name && e.reason === entry.reason)) {
      this.entries.push(entry);
    }
  }
}

// recordFailedRequest files a failed_requests entry (from firstPartyFailure;
// null is ignored): a request that started before the sign-in ended is the
// sign-in's (c.signIn.failedRequests), not the check's.
export function recordFailedRequest(c, entry, startedDuringSignIn) {
  if (!entry) {
    return;
  }
  const list = startedDuringSignIn && c.signIn ? c.signIn.failedRequests : c.failedRequests;
  if (list.length < CAPS.failedRequests) {
    list.push(entry);
  }
}

// secretForms are the forms of sign_in's password redactText replaces: as
// it is and URL-encoded (a form sent with GET puts it in a URL). Empty
// without sign_in.
let secretForms = [];

// setSecret sets the password redactText replaces ("" for none). run.mjs
// sets it once the input is read, so that every string is redacted before
// capString or oneLine shortens or reshapes it: a password cut by a cap would
// no longer be recognised, here or by cmd/verify-browser.
export function setSecret(secret) {
  secretForms = secret ? [...new Set([secret, encodeURIComponent(secret), new URLSearchParams({ p: secret }).toString().slice(2)])] : [];
}

// redactText replaces the password set by setSecret in s with "<redacted>".
export function redactText(s) {
  return secretForms.reduce((acc, f) => acc.split(f).join('<redacted>'), String(s ?? ''));
}

// redactSecret replaces the password set by setSecret in every string value
// and object key of v, at any depth. cmd/verify-browser redacts it as it is
// again before writing the result.
export function redactSecret(v) {
  if (secretForms.length === 0) {
    return v;
  }
  const walk = (x) => {
    if (typeof x === 'string') {
      return redactText(x);
    }
    if (Array.isArray(x)) {
      return x.map(walk);
    }
    if (x !== null && typeof x === 'object') {
      return Object.fromEntries(Object.entries(x).map(([k, val]) => [redactText(k), walk(val)]));
    }
    return x;
  };
  return walk(v);
}

// READ_ONLY_METHODS are the methods a read_only check lets through: the ones
// that by HTTP's definition change nothing.
export const READ_ONLY_METHODS = Object.freeze(['GET', 'HEAD', 'OPTIONS']);

// isReadOnlyMethod reports whether a read_only check lets a request with this
// method be sent. Methods compare case-insensitively; anything that isn't a
// string naming one of READ_ONLY_METHODS is blocked.
export function isReadOnlyMethod(method) {
  return typeof method === 'string' && READ_ONLY_METHODS.includes(method.trim().toUpperCase());
}

// blockedRequestEntry is the blocked_requests entry for a request read_only
// refused to send.
export function blockedRequestEntry(url, method) {
  return { url: capString(String(url ?? ''), CAPS.urlChars), method: capString(String(method ?? ''), CAPS.detailChars) };
}

// blockedByReadOnly reports whether a failed request is one read_only
// refused: in a read_only check every request with any other method is
// failed before it is sent, so its failure is reported in blocked_requests,
// not failed_requests.
export function blockedByReadOnly(method, readOnly) {
  return readOnly === true && !isReadOnlyMethod(method);
}

// capString truncates s to max characters (code points), marking the cut,
// after redacting the password.
export function capString(s, max) {
  const str = redactText(s);
  const chars = [...str];
  if (chars.length <= max) {
    return str;
  }
  return chars.slice(0, max - 1).join('') + '…';
}

// oneLine collapses whitespace and strips ANSI colour codes, for Playwright
// error messages that carry a multi-line call log. The password is redacted
// first.
export function oneLine(s) {
  // eslint-disable-next-line no-control-regex
  return redactText(s).replace(/\u001b\[[0-9;]*m/g, '').replace(/\s+/g, ' ').trim();
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
// there are no page errors and no failed first-party requests; with sign_in,
// only when it signed in. Console errors, timings, blocked navigations and
// the sign-in's refused cookies on their own never decide it (a blocked
// navigation fails the step that caused it).
export function computeVerdict(r) {
  const ok =
    Number.isInteger(r.http_status) &&
    r.http_status >= 200 &&
    r.http_status < 300 &&
    r.steps.every((s) => s.ok) &&
    r.assertions.every((a) => a.passed) &&
    r.page_errors.length === 0 &&
    r.failed_requests.length === 0 &&
    (r.sign_in === undefined || r.sign_in.signed_in === true);
  return ok ? 'pass' : 'fail';
}

function roundTiming(v) {
  return typeof v === 'number' && Number.isFinite(v) && v >= 0 ? Math.round(v) : undefined;
}

// buildResult assembles a completed result.json from what the browser
// collected, applying every cap and the verdict. With c.signIn it adds
// sign_in; when the sign-in failed, every step and assertion is reported as
// not run. With c.screenshot (set only when the input asked for one) it adds
// screenshot, {width, height}, or screenshot_error when c.screenshot has an
// error.
export function buildResult(c) {
  const result = {
    schema_version: SCHEMA_VERSION,
    status: 'completed',
    verdict: 'fail',
    http_status: Number.isInteger(c.httpStatus) ? c.httpStatus : 0,
    final_url: capString(c.finalUrl ?? '', CAPS.urlChars),
    viewport: resultViewport(c.viewport),
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
    blocked_requests: (c.blockedRequests ?? []).slice(0, CAPS.blockedRequests),
    blocked_navigations: (c.blockedNavigations ?? [])
      .slice(0, CAPS.blockedNavigations)
      .map((u) => capString(u, CAPS.urlChars)),
    timings_ms: {},
    dom_text_snippet: capString(redactText(c.domText).trim(), CAPS.domTextChars),
    outline: buildOutline(c.outline, c.finalUrl),
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
  if (c.signIn) {
    result.sign_in = signInResult(c.signIn);
    if (!result.sign_in.signed_in) {
      result.steps = result.steps.map((s) => ({ ...s, ok: false, error: SIGN_IN_NOT_RUN }));
      result.assertions = result.assertions.map((a) => ({ ...a, passed: false, detail: SIGN_IN_NOT_RUN }));
    }
  }
  if (c.screenshot) {
    if (c.screenshot.error) {
      result.screenshot_error = c.screenshot.error;
    } else {
      result.screenshot = { width: c.screenshot.width, height: c.screenshot.height };
    }
  }
  result.verdict = computeVerdict(result);
  return result;
}

// signInResult is the result's sign_in: reason only when not signed in,
// signed_out_on_load only when signed in.
function signInResult(s) {
  const out = { signed_in: s.signedIn === true };
  if (out.signed_in) {
    out.signed_out_on_load = s.signedOutOnLoad === true;
  } else {
    out.reason = capString(oneLine(s.reason), CAPS.detailChars);
  }
  out.failed_requests = (s.failedRequests ?? []).slice(0, CAPS.failedRequests);
  out.refused_cookies = (s.refusedCookies ?? []).slice(0, CAPS.refusedCookies);
  return out;
}

// FIELD_ROLES are ARIA roles that make an element a form field.
const FIELD_ROLES = new Set(['textbox', 'searchbox', 'combobox', 'listbox', 'checkbox', 'radio', 'switch', 'slider', 'spinbutton']);
// BUTTON_INPUT_TYPES are <input> types that render as a button.
const BUTTON_INPUT_TYPES = new Set(['submit', 'button', 'reset', 'image']);

function firstText(...values) {
  for (const v of values) {
    const t = oneLine(v);
    if (t !== '') {
      return t;
    }
  }
  return '';
}

// defaultButtonText is the caption a browser draws on an <input type=submit>
// or <input type=reset> with no value. It is not in innerText either, so
// without it the outline would drop a real form action.
function defaultButtonText(el) {
  if (String(el.tag ?? '').trim().toLowerCase() !== 'input') {
    return '';
  }
  const type = String(el.type ?? '').trim().toLowerCase();
  return type === 'submit' ? 'Submit' : type === 'reset' ? 'Reset' : '';
}

// outlineKind classifies one raw element: an explicit role wins over the tag
// (an <a role="button"> is a button). It returns '' for anything that is not
// a heading, link, button or field.
function outlineKind(el) {
  const role = String(el.role ?? '').trim().toLowerCase();
  const tag = String(el.tag ?? '').trim().toLowerCase();
  const type = String(el.type ?? '').trim().toLowerCase();
  if (role === 'heading' || role === 'link' || role === 'button') {
    return role;
  }
  if (FIELD_ROLES.has(role)) {
    return 'field';
  }
  if (/^h[1-6]$/.test(tag)) {
    return 'heading';
  }
  if (tag === 'a') {
    return el.href ? 'link' : '';
  }
  if (tag === 'button') {
    return 'button';
  }
  if (tag === 'input') {
    if (type === 'hidden') {
      return '';
    }
    return BUTTON_INPUT_TYPES.has(type) ? 'button' : 'field';
  }
  if (tag === 'select' || tag === 'textarea') {
    return 'field';
  }
  return '';
}

// headingLevel is aria-level when it is 1–6 (it overrides the tag, as in the
// accessibility tree), else the hN tag's number, else ARIA's default of 2.
function headingLevel(el) {
  const aria = Number(el.level);
  if (Number.isInteger(aria) && aria >= 1 && aria <= 6) {
    return aria;
  }
  const fromTag = /^h([1-6])$/.exec(String(el.tag ?? '').trim().toLowerCase());
  return fromTag ? Number(fromTag[1]) : 2;
}

// OUTLINE_HREF_SCHEMES are the schemes a link's href is reported with: web
// pages, plus mailto: and tel:, whose href IS the link's destination (the
// outline only reports it, never follows it). Anything else — javascript:,
// data:, blob: and the like — is script or inline content, not a destination,
// and the link is reported by its text alone.
const OUTLINE_HREF_SCHEMES = new Set(['http:', 'https:', 'mailto:', 'tel:']);

// resolveHref resolves a link's href against the page URL, keeping it only
// for OUTLINE_HREF_SCHEMES.
function resolveHref(href, base) {
  if (String(href ?? '').trim() === '') {
    return '';
  }
  try {
    const u = new URL(String(href), String(base ?? ''));
    return OUTLINE_HREF_SCHEMES.has(u.protocol) ? capString(u.href, CAPS.urlChars) : '';
  } catch {
    return '';
  }
}

// buildOutline turns the raw elements run.mjs collected from the final page
// (in document order) into the result's outline: visible headings, links,
// buttons and form fields, each {kind, text} plus level for headings and the
// resolved href for links. A field's text is its label: a <label>, then
// aria-label, placeholder, name. Hidden elements, and entries with nothing to
// show, are dropped; at most CAPS.outlineEntries are kept. Page content is
// untrusted: this only normalises and caps it.
export function buildOutline(raw, baseUrl) {
  const out = [];
  for (const el of Array.isArray(raw) ? raw : []) {
    if (out.length >= CAPS.outlineEntries) {
      break;
    }
    if (el === null || typeof el !== 'object' || el.hidden === true) {
      continue;
    }
    const kind = outlineKind(el);
    let entry;
    switch (kind) {
      case 'heading':
        entry = { kind, text: firstText(el.text, el.ariaLabel), level: headingLevel(el) };
        break;
      case 'link':
        entry = { kind, text: firstText(el.text, el.ariaLabel, el.alt, el.title), href: resolveHref(el.href ?? '', baseUrl) };
        if (entry.text === '' && entry.href === '') {
          continue;
        }
        break;
      case 'button':
        entry = { kind, text: firstText(el.text, el.ariaLabel, el.value, el.alt, el.title, defaultButtonText(el)) };
        break;
      case 'field':
        entry = { kind, text: firstText(el.labelText, el.ariaLabel, el.placeholder, el.name) };
        break;
      default:
        continue;
    }
    if (entry.text === '' && kind !== 'link') {
      continue;
    }
    entry.text = capString(entry.text, CAPS.outlineTextChars);
    if (kind === 'link' && entry.href === '') {
      delete entry.href;
    }
    out.push(entry);
  }
  return out;
}

// errorResult is result.json for a check that could not run, with the
// requests read_only refused before it stopped.
export function errorResult(message, blockedRequests = []) {
  return {
    schema_version: SCHEMA_VERSION,
    status: 'error',
    error: capString(oneLine(message), CAPS.detailChars * 3),
    blocked_requests: blockedRequests.slice(0, CAPS.blockedRequests),
  };
}

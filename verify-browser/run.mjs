// run.mjs drives headless Chromium for one verify-browser check.
//
//   node run.mjs <input.json> <result path> <proxy host:port>
//
// cmd/verify-browser starts it after validating the input and starting the
// egress proxy, and renames the result into place once this exits. The
// contract is docs/VERIFY_BROWSER.md; the pure logic is in lib.mjs.
//
// What keeps the page in its lane:
//   - every request goes through the proxy (loopback is not bypassed), which
//     refuses private, link-local, loopback and CGN addresses and IP literals;
//   - main-frame navigations — link clicks, redirects, gotos — to a host
//     outside allowed_hosts are failed before they are sent, through the
//     DevTools Fetch domain, which (unlike page.route) also sees redirect hops;
//   - new windows are refused in the page (window.open, links and forms
//     with a target) and, for any that still get created, closed by the
//     browser-level guard before their first navigation is let go; service
//     workers are blocked, downloads refused and dialogs dismissed;
//   - with read_only, every request whose method is not GET, HEAD or OPTIONS
//     is failed before it is sent and no WebSocket opens (guardReadOnly) —
//     in workers too, which still run.

import { readFile, writeFile } from 'node:fs/promises';
import { setTimeout as sleep } from 'node:timers/promises';
import { chromium, devices } from 'playwright-core';
import {
  CAPS,
  LIMITS,
  READ_ONLY_METHODS,
  READ_ONLY_WORKER_MARKER,
  abortedByNavigation,
  blameBlockedNavigation,
  blockedByReadOnly,
  blockedRequestEntry,
  buildResult,
  errorResult,
  firstPartyFailure,
  isAllowedUrl,
  isReadOnlyMethod,
  normalizeInput,
  oneLine,
  pushCapped,
  readOnlyWorkerScript,
  readOnlyWorkerWrapper,
  viewportContextOptions,
} from './lib.mjs';

// The script finishes this long before timeout_ms (at most), leaving time to
// close the browser and write the result before cmd/verify-browser's SIGTERM.
const MAX_WRAP_UP_MS = 5000;
const LAUNCH_TIMEOUT_MS = 30000;
const FIRST_LOAD_TIMEOUT_MS = 30000;
const LOAD_EVENT_WAIT_MS = 5000;
const NETWORK_IDLE_WAIT_MS = 3000;
const STEP_SETTLE_MS = 300;
const ASSERTION_WAIT_MS = 3000;
const CLOSE_TIMEOUT_MS = 2000;
const EVALUATE_TIMEOUT_MS = 1500;

// OUTLINE_RAW_MAX bounds how many visible elements readOutline hands back;
// lib.mjs's buildOutline keeps CAPS.outlineEntries of them.
const OUTLINE_RAW_MAX = 200;
const POPUP_BINDING = '__verifyBrowserBlockedPopup';
const READ_ONLY_BINDING = '__verifyBrowserBlockedRequest';
const READ_ONLY_CHANNEL = '__verifyBrowserBlockedRequest';
const READ_ONLY_INSTALLED = '__verifyBrowserReadOnly';
// READ_ONLY_REFUSED_URL is where a worker's refused XMLHttpRequest goes
// instead (a reserved name: it never resolves).
const READ_ONLY_REFUSED_URL = 'https://read-only.verify-browser.invalid/';

async function main() {
  const [inputPath, resultPath, proxyAddr] = process.argv.slice(2);
  if (!inputPath || !resultPath || !proxyAddr) {
    throw new Error('usage: node run.mjs <input.json> <result path> <proxy host:port>');
  }
  const started = Date.now();
  const c = newCollector();
  let result;
  try {
    const input = normalizeInput(JSON.parse(await readFile(inputPath, 'utf8')));
    const wrapUp = Math.min(MAX_WRAP_UP_MS, Math.floor(input.timeoutMs / 4));
    result = await check(input, proxyAddr, started + input.timeoutMs - wrapUp, c);
  } catch (err) {
    result = errorResult(errorText(err), c.blockedRequests);
  }
  await writeFile(resultPath, JSON.stringify(result));
}

function errorText(err) {
  return oneLine(err?.message ?? String(err));
}

// withTimeout resolves to fallback when promise hasn't settled within ms, or
// when it rejects.
async function withTimeout(promise, ms, fallback) {
  return Promise.race([promise.catch(() => fallback), sleep(ms, fallback)]);
}

async function check(input, proxyAddr, deadline, c) {
  const remaining = () => deadline - Date.now();
  const browser = await chromium.launch({
    headless: true,
    chromiumSandbox: false,
    proxy: { server: `http://${proxyAddr}` },
    args: [
      '--no-sandbox',
      '--proxy-bypass-list=<-loopback>',
      '--disable-quic',
      '--force-webrtc-ip-handling-policy=disable_non_proxied_udp',
    ],
    timeout: Math.max(1, Math.min(LAUNCH_TIMEOUT_MS, remaining())),
  });
  try {
    return await checkInBrowser(browser, input, remaining, c);
  } finally {
    await withTimeout(browser.close(), CLOSE_TIMEOUT_MS, undefined);
  }
}

// newCollector returns what the browser saw, plus the blocked-navigation
// bookkeeping the steps use to tell that they caused one.
function newCollector() {
  const c = {
    httpStatus: 0,
    finalUrl: '',
    steps: [],
    assertions: [],
    pageErrors: [],
    consoleErrors: [],
    failedRequests: [],
    blockedRequests: [],
    blockedNavigations: [],
    timings: {},
    domText: '',
    outline: [],
    firstLoadError: '',
    blockedCount: 0,
    lastBlocked: '',
    // doc counts main-frame document loads (not same-document navigations);
    // stepRunning is true while a step runs, which then takes the blame
    // for any navigation blocked meanwhile.
    doc: 0,
    stepRunning: false,
  };
  c.blocked = (url) => {
    c.blockedCount++;
    c.lastBlocked = String(url);
    pushCapped(c.blockedNavigations, String(url), CAPS.blockedNavigations);
    if (!c.stepRunning) {
      blameBlockedNavigation(c.steps, c.doc, c.lastBlocked);
    }
  };
  // blockedRequest records a request read_only refused. Unlike a blocked
  // navigation it fails no step: the step did what it was asked, and the
  // request it caused is reported instead.
  c.blockedRequest = (url, method) => {
    if (c.blockedRequests.length < CAPS.blockedRequests) {
      c.blockedRequests.push(blockedRequestEntry(url, method));
    }
  };
  return c;
}

async function checkInBrowser(browser, input, remaining, c) {
  c.viewport = input.viewport;
  const allowed = (url) => isAllowedUrl(url, input.allowedHosts);
  const context = await browser.newContext({
    ...viewportContextOptions(input.viewport, devices),
    serviceWorkers: 'block',
    acceptDownloads: false,
  });
  await context.exposeBinding(POPUP_BINDING, (_source, url) => c.blocked(url));
  await context.addInitScript(popupGuard, POPUP_BINDING);
  if (input.readOnly) {
    await guardReadOnly(context, c.blockedRequest);
  }
  let page = null;
  context.on('page', (p) => {
    // The first page is ours; any later one is a window the page opened.
    if (page === null || p === page) {
      return;
    }
    const url = p.url();
    c.blocked(url && url !== 'about:blank' ? url : 'about:blank (a new window the page opened)');
    p.close().catch(() => {});
  });
  page = await context.newPage();
  const onReadOnlyBlocked = input.readOnly ? c.blockedRequest : null;
  const mainTargetId = await guardNavigations(context, page, allowed, c.blocked, () => c.doc++, onReadOnlyBlocked);
  await guardNewWindows(browser, mainTargetId, c.blocked);
  watchPage(page, input.allowedHosts, allowed, c, input.readOnly);

  const loaded = await firstLoad(page, input.url, remaining, c);
  if (!loaded && c.blockedCount === 0) {
    throw new Error(`the page could not be loaded: ${c.firstLoadError}`);
  }
  await runSteps(page, input.steps, remaining, c, loaded);
  for (const a of input.assertions) {
    c.assertions.push({ type: a.type, ...(await checkAssertion(page, a, remaining)) });
  }
  c.finalUrl = page.url();
  c.domText = await withTimeout(
    page.evaluate(() => (document.body ? document.body.innerText : '')),
    EVALUATE_TIMEOUT_MS,
    '',
  );
  c.outline = await withTimeout(page.evaluate(readOutline, OUTLINE_RAW_MAX), EVALUATE_TIMEOUT_MS, []);
  return buildResult(c);
}

// popupGuard runs in every frame before the page's scripts: window.open and
// links that would open a new window are refused and reported instead.
function popupGuard(binding) {
  const report = (u) => {
    try {
      window[binding](String(u));
    } catch {
      // The binding is best-effort; the context's page handler still closes
      // any window that gets through.
    }
  };
  // opensWindow reports whether a link or form with this target (its own,
  // else <base target>) would open a new window rather than stay in a frame.
  const opensWindow = (target) => {
    // The keywords are ASCII case-insensitive; a frame name matches exactly.
    const name = target ?? document.querySelector('base[target]')?.getAttribute('target') ?? '';
    const t = name.toLowerCase();
    if (t === '' || t === '_self' || t === '_top' || t === '_parent') {
      return false;
    }
    return t === '_blank' || !document.querySelector(`iframe[name="${CSS.escape(name)}"]`);
  };
  // window.open with _self, _top, _parent or an iframe's name navigates an
  // existing frame (main-frame ones are checked by guardNavigations); with
  // no target or any other one it opens a window, which is refused.
  const open = window.open;
  window.open = function guardedOpen(u, target, ...rest) {
    const t = target === undefined || target === null || String(target) === '' ? '_blank' : String(target);
    if (!opensWindow(t)) {
      return open.call(this, u, target, ...rest);
    }
    try {
      report(new URL(String(u ?? ''), location.href).href);
    } catch {
      report(String(u));
    }
    return null;
  };
  const onLink = (e) => {
    const a = e.target instanceof Element ? e.target.closest('a[href], area[href]') : null;
    if (a && opensWindow(a.getAttribute('target'))) {
      e.preventDefault();
      report(a.href);
    }
  };
  document.addEventListener('click', onLink, true);
  document.addEventListener('auxclick', onLink, true);
  const formOpensWindow = (form, submitter) =>
    opensWindow(submitter?.getAttribute?.('formtarget') ?? form.getAttribute('target'));
  const formAction = (form, submitter) => submitter?.formAction || form.action;
  document.addEventListener(
    'submit',
    (e) => {
      if (e.target instanceof HTMLFormElement && formOpensWindow(e.target, e.submitter)) {
        e.preventDefault();
        report(formAction(e.target, e.submitter));
      }
    },
    true,
  );
  // form.submit() fires no submit event.
  const submit = HTMLFormElement.prototype.submit;
  HTMLFormElement.prototype.submit = function guardedSubmit() {
    if (formOpensWindow(this, null)) {
      report(this.action);
      return;
    }
    submit.call(this);
  };
}

// guardNewWindows closes every new page target the page manages to open
// despite popupGuard (a path it doesn't cover). A browser-level auto-attach
// that waits for the debugger holds each new target's first navigation until
// every attached client lets it go; this one never does — it closes the
// target — so the window's first request is never sent. It is recorded as a
// blocked navigation; the context's page handler is the last backstop.
async function guardNewWindows(browser, mainTargetId, onBlocked) {
  const cdp = await browser.newBrowserCDPSession();
  cdp.on('Target.attachedToTarget', ({ targetInfo }) => {
    if (targetInfo.type !== 'page' || targetInfo.targetId === mainTargetId) {
      return;
    }
    const url = targetInfo.url;
    onBlocked(url && url !== 'about:blank' ? url : 'about:blank (a new window the page opened)');
    cdp.send('Target.closeTarget', { targetId: targetInfo.targetId }).catch(() => {});
  });
  await cdp.send('Target.setAutoAttach', {
    autoAttach: true,
    waitForDebuggerOnStart: true,
    flatten: true,
    filter: [{ type: 'page' }],
  });
}

// guardNavigations fails every main-frame document request whose URL is not
// allowed, before it is sent. The Fetch domain pauses each redirect hop as its
// own request, so a preview redirecting elsewhere is caught too.
// It calls onDocument for each new main-frame document (Page.frameNavigated
// is not sent for same-document navigations) and returns the page's target id.
//
// With onReadOnlyBlocked (read_only), it pauses every request of the page's
// target — any frame in its process, any resource type, any host, each
// redirect hop — and fails the ones whose method is not GET, HEAD or OPTIONS,
// reporting each to onReadOnlyBlocked. They are failed as Aborted, not
// BlockedByClient, so a blocked form submission leaves the page where it was
// rather than on an error page.
async function guardNavigations(context, page, allowed, onBlocked, onDocument, onReadOnlyBlocked) {
  const cdp = await context.newCDPSession(page);
  const { targetInfo } = await cdp.send('Target.getTargetInfo');
  const { frameTree } = await cdp.send('Page.getFrameTree');
  const mainFrameId = frameTree.frame.id;
  cdp.on('Page.frameNavigated', ({ frame }) => {
    if (frame.id === mainFrameId) {
      onDocument();
    }
  });
  await cdp.send('Page.enable');
  cdp.on('Fetch.requestPaused', (ev) => {
    if (onReadOnlyBlocked && !isReadOnlyMethod(ev.request.method)) {
      onReadOnlyBlocked(ev.request.url, ev.request.method);
      cdp.send('Fetch.failRequest', { requestId: ev.requestId, errorReason: 'Aborted' }).catch(() => {});
      return;
    }
    if (ev.frameId === mainFrameId && ev.resourceType === 'Document' && !allowed(ev.request.url)) {
      onBlocked(ev.request.url);
      cdp.send('Fetch.failRequest', { requestId: ev.requestId, errorReason: 'BlockedByClient' }).catch(() => {});
      return;
    }
    cdp.send('Fetch.continueRequest', { requestId: ev.requestId }).catch(() => {});
  });
  const pattern = onReadOnlyBlocked
    ? { urlPattern: '*', requestStage: 'Request' }
    : { urlPattern: '*', resourceType: 'Document', requestStage: 'Request' };
  await cdp.send('Fetch.enable', { patterns: [pattern] });
  return targetInfo.targetId;
}

// guardReadOnly holds a read_only check to its rule everywhere guardNavigations
// cannot see. A context route fails every request with another method that
// any frame — out-of-process ones included — or dedicated worker makes
// (Playwright attaches to each of their targets and intercepts there before
// letting it run). readOnlyGuard, run in every frame, refuses WebSockets
// (which no request interception sees) and hands each worker a guarded copy
// of itself: the page marks an http(s) worker's script URL, and the route
// serves readOnlyWorkerWrapper for it, which runs readOnlyGuard in the worker
// before the worker's own script. Each refusal is reported to onBlocked once
// (a worker's report reaches every same-origin frame of the page).
async function guardReadOnly(context, onBlocked) {
  const reported = new Set();
  await context.exposeBinding(READ_ONLY_BINDING, (_source, url, method, id) => {
    if (id !== undefined) {
      if (reported.has(id)) {
        return;
      }
      reported.add(id);
    }
    onBlocked(url, method);
  });
  const options = {
    binding: READ_ONLY_BINDING,
    channel: READ_ONLY_CHANNEL,
    installed: READ_ONLY_INSTALLED,
    marker: READ_ONLY_WORKER_MARKER,
    methods: [...READ_ONLY_METHODS],
    refusedUrl: READ_ONLY_REFUSED_URL,
    source: readOnlyGuard.toString(),
    inWorker: false,
  };
  await context.addInitScript(readOnlyGuard, options);
  const workerGuard = `(${options.source})(${JSON.stringify({ ...options, inWorker: true })});`;
  await context.route('**/*', (route) => {
    const req = route.request();
    if (req.url().startsWith(READ_ONLY_REFUSED_URL)) {
      // A worker's refused XMLHttpRequest, already reported by its guard.
      return route.abort('failed');
    }
    if (!isReadOnlyMethod(req.method())) {
      onBlocked(req.url(), req.method());
      return route.abort('aborted');
    }
    const worker = readOnlyWorkerScript(req.url());
    if (worker) {
      return route.fulfill({
        status: 200,
        contentType: 'text/javascript',
        body: readOnlyWorkerWrapper(workerGuard, worker.scriptUrl, worker.type),
      });
    }
    return route.fallback();
  });
}

// readOnlyGuard runs before any of the page's own scripts in every frame of a
// read_only check (inWorker false) and in every worker (inWorker true):
//   - WebSocket and WebSocketStream refuse to connect, reported as
//     "WEBSOCKET" rather than thrown, so the page carries on as it would with
//     a server that never answered;
//   - Worker and SharedWorker still start the worker, guarded: an http(s)
//     worker created by a frame gets the marked script URL (worker scripts'
//     requests reach the route, and the marked one goes no further) that the
//     context route answers with a wrapper; a blob: or data: worker, or one a worker creates
//     (whose script no route sees), is started from a blob that runs this
//     guard and then loads the script. In such a blob, fetch, Request,
//     XMLHttpRequest, importScripts, EventSource and nested workers resolve
//     relative URLs against the script's own http(s) URL (base), as they
//     would in the worker run natively. Each real constructor's prototype
//     points back at the guarded one, so the page can't reach the real one;
//   - in a worker, fetch and XMLHttpRequest refuse any method but GET, HEAD
//     and OPTIONS, since a shared or nested worker's requests reach no route
//     (a refused XMLHttpRequest is sent as a GET to refusedUrl, which fails as
//     a network error would).
// A frame reports through the binding; a worker, which has none, posts to a
// BroadcastChannel every same-origin frame forwards to it. Reports carry an
// id so guardReadOnly counts each once.
function readOnlyGuard(o) {
  const g = globalThis;
  if (g[o.installed]) {
    return;
  }
  try {
    Object.defineProperty(g, o.installed, { value: true });
  } catch {
    // Installing twice is harmless.
  }
  const absolute = (u) => {
    try {
      return new URL(String(u), o.base ?? location.href).href;
    } catch {
      return String(u);
    }
  };
  // The URL a worker run from a blob would have had natively.
  const base = typeof o.base === 'string' && /^https?:/.test(o.base) ? o.base : null;
  const here = () => base ?? location.href;
  const resolve = (u) => {
    if (!base || typeof u !== 'string') {
      return u;
    }
    try {
      return new URL(u, base).href;
    } catch {
      return u;
    }
  };
  let seq = 0;
  let channel = null;
  const report = (u, method) => {
    const id = `${Date.now().toString(36)}.${Math.random().toString(36).slice(2)}.${seq++}`;
    try {
      if (o.inWorker) {
        channel ??= new BroadcastChannel(o.channel);
        channel.postMessage({ url: absolute(u), method: String(method), id });
      } else {
        g[o.binding](absolute(u), String(method), id);
      }
    } catch {
      // Best-effort: the request is refused either way.
    }
  };
  if (!o.inWorker && typeof BroadcastChannel === 'function') {
    try {
      new BroadcastChannel(o.channel).onmessage = (e) => {
        const d = e.data;
        if (d && typeof d === 'object') {
          try {
            g[o.binding](String(d.url), String(d.method), String(d.id));
          } catch {
            // Best-effort, like report.
          }
        }
      };
    } catch {
      // An opaque-origin frame has no channel; its workers' reports are lost.
    }
  }
  const define = (name, value) => {
    try {
      Object.defineProperty(g, name, { value, writable: true, configurable: true });
    } catch {
      // A property that can't be redefined keeps its value.
    }
  };
  if (typeof g.WebSocket === 'function') {
    const Real = g.WebSocket;
    const Blocked = function WebSocket(u) {
      if (!new.target) {
        throw new TypeError("Failed to construct 'WebSocket': Please use the 'new' operator");
      }
      report(u, 'WEBSOCKET');
      const ws = new EventTarget();
      Object.defineProperties(ws, {
        url: { value: absolute(u) },
        readyState: { value: 3 },
        protocol: { value: '' },
        extensions: { value: '' },
        bufferedAmount: { value: 0 },
        binaryType: { value: 'blob', writable: true },
        send: { value: () => {} },
        close: { value: () => {} },
      });
      for (const h of ['onopen', 'onmessage', 'onerror', 'onclose']) {
        ws[h] = null;
      }
      setTimeout(() => {
        const err = new Event('error');
        ws.dispatchEvent(err);
        ws.onerror?.(err);
        const close = new CloseEvent('close', { code: 1006, reason: '', wasClean: false });
        ws.dispatchEvent(close);
        ws.onclose?.(close);
      }, 0);
      return ws;
    };
    for (const k of ['CONNECTING', 'OPEN', 'CLOSING', 'CLOSED']) {
      Object.defineProperty(Blocked, k, { value: Real[k] });
    }
    define('WebSocket', Blocked);
  }
  if (typeof g.WebSocketStream === 'function') {
    define('WebSocketStream', function WebSocketStream(u) {
      report(u, 'WEBSOCKET');
      throw new DOMException('WebSocket connections are blocked in a read-only check', 'SecurityError');
    });
  }
  for (const name of ['Worker', 'SharedWorker']) {
    const Real = g[name];
    if (typeof Real !== 'function') {
      continue;
    }
    const Guarded = function (u, options) {
      if (!new.target) {
        throw new TypeError(`Failed to construct '${name}': Please use the 'new' operator`);
      }
      const make = (url) => Reflect.construct(Real, [url, options], new.target === Guarded ? Real : new.target);
      let url;
      try {
        url = new URL(String(u), here());
      } catch {
        return make(u);
      }
      const type = options && typeof options === 'object' && options.type === 'module' ? 'module' : 'classic';
      if (!o.inWorker && (url.protocol === 'https:' || url.protocol === 'http:')) {
        url.search = `${url.search || '?'}${url.search ? '&' : ''}${o.marker}=${type}`;
        return make(url.href);
      }
      // base: the script's own URL, for reporting the blob's relative URLs.
      const guard = `(${o.source})(${JSON.stringify({ ...o, inWorker: true, base: url.href })});`;
      const load =
        type === 'module' ? `await import(${JSON.stringify(url.href)});` : `importScripts(${JSON.stringify(url.href)});`;
      return make(URL.createObjectURL(new Blob([`${guard}\n${load}\n`], { type: 'text/javascript' })));
    };
    Object.defineProperty(Guarded, 'name', { value: name });
    Guarded.prototype = Real.prototype;
    try {
      // Worker.prototype.constructor would otherwise be the unguarded Real.
      Object.defineProperty(Real.prototype, 'constructor', { value: Guarded, writable: true, configurable: true });
    } catch {
      // A prototype that can't be changed keeps its constructor.
    }
    define(name, Guarded);
  }
  if (!o.inWorker) {
    return;
  }
  const safe = (m) => typeof m === 'string' && o.methods.includes(m.trim().toUpperCase());
  const RealRequest = g.Request;
  if (base) {
    // Constructors that take a URL first: a subclass resolves it against base.
    for (const name of ['Request', 'EventSource']) {
      const Real = g[name];
      if (typeof Real !== 'function') {
        continue;
      }
      const Based = class extends Real {
        constructor(u, ...rest) {
          super(resolve(u), ...rest);
        }
      };
      Object.defineProperty(Based, 'name', { value: name });
      try {
        Object.defineProperty(Real.prototype, 'constructor', { value: Based, writable: true, configurable: true });
      } catch {
        // As for the workers above.
      }
      define(name, Based);
    }
    if (typeof g.importScripts === 'function') {
      const realImportScripts = g.importScripts;
      define('importScripts', function importScripts(...urls) {
        return realImportScripts.apply(this, urls.map(resolve));
      });
    }
  }
  if (typeof g.fetch === 'function') {
    const realFetch = g.fetch;
    define('fetch', function fetch(input, init) {
      const isRequest = typeof RealRequest === 'function' && input instanceof RealRequest;
      const method = init && init.method !== undefined ? String(init.method) : isRequest ? input.method : 'GET';
      if (!safe(method)) {
        report(isRequest ? input.url : input, method);
        return Promise.reject(new TypeError('Failed to fetch'));
      }
      return realFetch.call(this, resolve(input), init);
    });
  }
  if (typeof g.XMLHttpRequest === 'function') {
    const open = g.XMLHttpRequest.prototype.open;
    g.XMLHttpRequest.prototype.open = function (method, u, ...rest) {
      if (!safe(String(method))) {
        report(u, String(method));
        return open.call(this, 'GET', o.refusedUrl, ...rest);
      }
      return open.call(this, method, resolve(u), ...rest);
    };
  }
}

// watchPage collects what the page does. A first-party request cancelled
// with net::ERR_ABORTED only counts as navigation noise when a main-frame
// navigation to an allowed URL began after it was sent (the page unloading),
// or it was a navigation that a later navigation in its own frame
// superseded; anything else — the app aborting its own request, an iframe
// whose load was cancelled — is reported.
//
// In a read_only check, a request read_only refused is in blocked_requests and
// is not reported again as failed.
function watchPage(page, allowedHosts, allowed, c, readOnly) {
  let mainNavigations = 0;
  const frameNavigations = new WeakMap();
  const counts = (frame) => ({ main: mainNavigations, frame: frameNavigations.get(frame) ?? 0 });
  const sentAt = new WeakMap();
  page.on('request', (req) => {
    const frame = req.frame();
    if (req.isNavigationRequest()) {
      frameNavigations.set(frame, (frameNavigations.get(frame) ?? 0) + 1);
      if (frame === page.mainFrame() && allowed(req.url())) {
        mainNavigations++;
      }
    }
    sentAt.set(req, counts(frame));
  });
  page.on('pageerror', (err) => {
    const text = err?.name && err?.message ? `${err.name}: ${err.message}` : String(err?.message ?? err);
    pushCapped(c.pageErrors, text, CAPS.pageErrors);
  });
  page.on('console', (msg) => {
    if (msg.type() === 'error') {
      pushCapped(c.consoleErrors, msg.text(), CAPS.consoleErrors);
    }
  });
  page.on('requestfailed', (req) => {
    if (blockedByReadOnly(req.method(), readOnly)) {
      return;
    }
    const now = counts(req.frame());
    const aborted = abortedByNavigation({ isNavigation: req.isNavigationRequest(), sent: sentAt.get(req) ?? now, now });
    const entry = firstPartyFailure(
      {
        url: req.url(),
        method: req.method(),
        status: 0,
        failure: req.failure()?.errorText ?? 'failed',
        abortedByNavigation: aborted,
      },
      allowedHosts,
    );
    if (entry && c.failedRequests.length < CAPS.failedRequests) {
      c.failedRequests.push(entry);
    }
  });
  page.on('response', (resp) => {
    const entry = firstPartyFailure(
      { url: resp.url(), method: resp.request().method(), status: resp.status(), failure: '' },
      allowedHosts,
    );
    if (entry && c.failedRequests.length < CAPS.failedRequests) {
      c.failedRequests.push(entry);
    }
  });
  page.on('dialog', (d) => {
    d.dismiss().catch(() => {});
  });
}

// firstLoad opens url and waits for it to settle. It returns false when the
// navigation failed (c.firstLoadError says why).
async function firstLoad(page, url, remaining, c) {
  try {
    const resp = await page.goto(url, {
      waitUntil: 'domcontentloaded',
      timeout: Math.max(1, Math.min(FIRST_LOAD_TIMEOUT_MS, remaining())),
    });
    c.httpStatus = resp ? resp.status() : 0;
  } catch (err) {
    c.firstLoadError = errorText(err);
    return false;
  }
  await waitQuietly(page, 'load', LOAD_EVENT_WAIT_MS, remaining);
  await waitQuietly(page, 'networkidle', NETWORK_IDLE_WAIT_MS, remaining);
  c.timings = await withTimeout(page.evaluate(readTimings), EVALUATE_TIMEOUT_MS, {});
  return true;
}

async function waitQuietly(page, state, ms, remaining) {
  const t = Math.min(ms, remaining());
  if (t > 0) {
    await page.waitForLoadState(state, { timeout: t }).catch(() => {});
  }
}

// readOutline runs in the page after the steps and assertions: the raw
// headings, links, buttons and form fields in document order, each with what
// buildOutline needs to classify, label and cap it. Visibility is decided
// here, where layout is known, and passed on as `hidden`.
function readOutline(max) {
  const selector = [
    'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'a', 'button', 'input', 'select', 'textarea',
    ...['heading', 'link', 'button', 'textbox', 'searchbox', 'combobox', 'listbox', 'checkbox', 'radio', 'switch', 'slider', 'spinbutton'].map(
      (r) => `[role="${r}"]`,
    ),
  ].join(',');
  const isHidden = (el) => {
    if (el.closest('[aria-hidden="true"], [hidden]')) {
      return true;
    }
    if (typeof el.checkVisibility === 'function') {
      return !el.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true });
    }
    return el.getClientRects().length === 0;
  };
  const text = (el) => (el && typeof el.innerText === 'string' ? el.innerText : '');
  const labelledBy = (el) =>
    (el.getAttribute('aria-labelledby') || '')
      .split(/\s+/)
      .map((id) => text(id ? document.getElementById(id) : null))
      .join(' ');
  const out = [];
  let visible = 0;
  let hidden = 0;
  for (const el of document.querySelectorAll(selector)) {
    if (visible >= max) {
      break;
    }
    const h = isHidden(el);
    if (h) {
      hidden++;
      if (hidden > max) {
        continue;
      }
    } else {
      visible++;
    }
    const tag = el.tagName.toLowerCase();
    const rawHref = el.getAttribute('href');
    out.push({
      tag,
      role: el.getAttribute('role') || '',
      type: el.getAttribute('type') || '',
      level: el.getAttribute('aria-level') || '',
      text: tag === 'select' || tag === 'textarea' ? '' : text(el),
      ariaLabel: el.getAttribute('aria-label') || labelledBy(el),
      labelText: el.labels && el.labels.length > 0 ? text(el.labels[0]) : '',
      placeholder: el.getAttribute('placeholder') || '',
      name: el.getAttribute('name') || '',
      value: tag === 'input' ? el.getAttribute('value') || '' : '',
      alt: el.getAttribute('alt') || (el.querySelector && el.querySelector('img[alt]')?.getAttribute('alt')) || '',
      title: el.getAttribute('title') || '',
      href: rawHref === null ? '' : typeof el.href === 'string' ? el.href : rawHref,
      hidden: h,
    });
  }
  return out;
}

// readTimings runs in the page: Navigation Timing and the latest Largest
// Contentful Paint entry from a buffered observer (none in some pages).
function readTimings() {
  return new Promise((resolve) => {
    const nav = performance.getEntriesByType('navigation')[0];
    const out = {
      domContentLoaded: nav && nav.domContentLoadedEventEnd > 0 ? nav.domContentLoadedEventEnd : undefined,
      load: nav && nav.loadEventEnd > 0 ? nav.loadEventEnd : undefined,
    };
    try {
      let lcp;
      const po = new PerformanceObserver((list) => {
        for (const e of list.getEntries()) {
          lcp = e.startTime;
        }
      });
      po.observe({ type: 'largest-contentful-paint', buffered: true });
      setTimeout(() => {
        po.disconnect();
        out.lcp = lcp;
        resolve(out);
      }, 100);
    } catch {
      resolve(out);
    }
  });
}

// runSteps runs the steps in order. Each step — the action, the settle pause
// and the load wait — gets 10 s or what remains of timeout_ms, whichever is
// less. A navigation blocked while a step runs fails that step; one blocked
// later is blamed by c.blocked on the steps that ran on its page.
async function runSteps(page, steps, remaining, c, loaded) {
  let stopped = loaded ? '' : 'not run: the first page load failed';
  for (const [i, step] of steps.entries()) {
    if (stopped) {
      c.steps.push({ action: step.action, ok: false, error: stopped, startDoc: c.doc, endDoc: c.doc });
      continue;
    }
    const before = c.blockedCount;
    const startDoc = c.doc;
    const stepDeadline = Date.now() + Math.min(LIMITS.stepTimeoutMs, remaining());
    const stepRemaining = () => Math.min(stepDeadline - Date.now(), remaining());
    c.stepRunning = true;
    let error = await runStep(page, step, stepRemaining);
    if (!error) {
      await sleep(Math.max(0, Math.min(STEP_SETTLE_MS, stepRemaining())));
      await waitQuietly(page, 'load', LOAD_EVENT_WAIT_MS, stepRemaining);
    }
    if (c.blockedCount > before) {
      error = `navigation to ${c.lastBlocked} was blocked: it is outside this Task's previews`;
    }
    c.steps.push({ action: step.action, ok: !error, error: error ?? '', startDoc, endDoc: c.doc });
    c.stepRunning = false;
    if (error) {
      stopped = `not run: step ${i} failed`;
    }
  }
}

// runStep performs one step within remaining() and returns null, or why it
// failed.
async function runStep(page, step, remaining) {
  const timeout = remaining();
  if (timeout <= 0) {
    return 'not run: timeout_ms was used up';
  }
  try {
    switch (step.action) {
      case 'goto':
        // normalizeInput checked the host; guardNavigations enforces it again
        // for every redirect the goto follows.
        await page.goto(step.url, { waitUntil: 'domcontentloaded', timeout });
        break;
      case 'click':
        await page.click(step.selector, { timeout });
        break;
      case 'fill':
        await page.fill(step.selector, step.value, { timeout });
        break;
      case 'press':
        await page.press(step.selector, step.key, { timeout });
        break;
      case 'wait_for': {
        const target = step.selector !== undefined ? page.locator(step.selector) : page.getByText(step.text);
        await target.filter({ visible: true }).first().waitFor({ state: 'visible', timeout });
        break;
      }
      default:
        return `unknown action ${step.action}`;
    }
    return null;
  } catch (err) {
    return errorText(err);
  }
}

async function checkAssertion(page, a, remaining) {
  try {
    switch (a.type) {
      case 'text_visible': {
        const ok = await visibleWithin(page.getByText(a.text), remaining);
        return { passed: ok, detail: ok ? 'visible' : 'not visible on the page' };
      }
      case 'text_absent': {
        const n = await page.getByText(a.text).filter({ visible: true }).count();
        return { passed: n === 0, detail: n === 0 ? 'not visible on the page' : `visible in ${n} element(s)` };
      }
      case 'selector_visible': {
        const ok = await visibleWithin(page.locator(a.selector), remaining);
        return { passed: ok, detail: ok ? 'visible' : 'no visible element matches' };
      }
      case 'url_path': {
        const path = new URL(page.url()).pathname;
        return { passed: path === a.path, detail: `path is ${path}` };
      }
      default:
        return { passed: false, detail: `unknown type ${a.type}` };
    }
  } catch (err) {
    return { passed: false, detail: errorText(err) };
  }
}

// visibleWithin waits briefly for a visible match — content a client-side
// app renders just after load still counts — and checks once when the time
// budget is spent.
async function visibleWithin(locator, remaining) {
  const visible = locator.filter({ visible: true }).first();
  const t = Math.min(ASSERTION_WAIT_MS, remaining());
  if (t < 50) {
    return (await visible.count()) > 0;
  }
  try {
    await visible.waitFor({ state: 'visible', timeout: t });
    return true;
  } catch (err) {
    if (err?.name === 'TimeoutError') {
      return false;
    }
    throw err;
  }
}

main().then(
  () => process.exit(0),
  (err) => {
    console.error(err);
    process.exit(1);
  },
);

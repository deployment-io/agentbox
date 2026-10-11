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
//     workers are blocked, downloads refused and dialogs dismissed.

import { readFile, writeFile } from 'node:fs/promises';
import { setTimeout as sleep } from 'node:timers/promises';
import { chromium } from 'playwright-core';
import {
  CAPS,
  LIMITS,
  abortedByNavigation,
  blameBlockedNavigation,
  buildResult,
  errorResult,
  firstPartyFailure,
  isAllowedUrl,
  normalizeInput,
  oneLine,
  pushCapped,
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

const POPUP_BINDING = '__verifyBrowserBlockedPopup';

async function main() {
  const [inputPath, resultPath, proxyAddr] = process.argv.slice(2);
  if (!inputPath || !resultPath || !proxyAddr) {
    throw new Error('usage: node run.mjs <input.json> <result path> <proxy host:port>');
  }
  const started = Date.now();
  let result;
  try {
    const input = normalizeInput(JSON.parse(await readFile(inputPath, 'utf8')));
    const wrapUp = Math.min(MAX_WRAP_UP_MS, Math.floor(input.timeoutMs / 4));
    result = await check(input, proxyAddr, started + input.timeoutMs - wrapUp);
  } catch (err) {
    result = errorResult(errorText(err));
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

async function check(input, proxyAddr, deadline) {
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
    return await checkInBrowser(browser, input, remaining);
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
    blockedNavigations: [],
    timings: {},
    domText: '',
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
  return c;
}

async function checkInBrowser(browser, input, remaining) {
  const c = newCollector();
  const allowed = (url) => isAllowedUrl(url, input.allowedHosts);
  const context = await browser.newContext({ serviceWorkers: 'block', acceptDownloads: false });
  await context.exposeBinding(POPUP_BINDING, (_source, url) => c.blocked(url));
  await context.addInitScript(popupGuard, POPUP_BINDING);
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
  const mainTargetId = await guardNavigations(context, page, allowed, c.blocked, () => c.doc++);
  await guardNewWindows(browser, mainTargetId, c.blocked);
  watchPage(page, input.allowedHosts, allowed, c);

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
async function guardNavigations(context, page, allowed, onBlocked, onDocument) {
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
    if (ev.frameId === mainFrameId && ev.resourceType === 'Document' && !allowed(ev.request.url)) {
      onBlocked(ev.request.url);
      cdp.send('Fetch.failRequest', { requestId: ev.requestId, errorReason: 'BlockedByClient' }).catch(() => {});
      return;
    }
    cdp.send('Fetch.continueRequest', { requestId: ev.requestId }).catch(() => {});
  });
  await cdp.send('Fetch.enable', {
    patterns: [{ urlPattern: '*', resourceType: 'Document', requestStage: 'Request' }],
  });
  return targetInfo.targetId;
}

// watchPage collects what the page does. A first-party request cancelled
// with net::ERR_ABORTED only counts as navigation noise when a main-frame
// navigation to an allowed URL began after it was sent (the page unloading),
// or it was a navigation that a later navigation in its own frame
// superseded; anything else — the app aborting its own request, an iframe
// whose load was cancelled — is reported.
function watchPage(page, allowedHosts, allowed, c) {
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

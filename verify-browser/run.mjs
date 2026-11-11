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
//     is failed before it is sent by the browser's own request interception
//     (guardNavigations, guardReadOnly), and the channels it can't see —
//     WebSocket, SharedWorker, WebTransport, RTCPeerConnection — are refused.
//
// With sign_in, the browser signs in with a test user through the app's own
// password form before the check's first load (signIn). The password is typed
// into the field and goes nowhere else: no error, step or console line quotes
// it, and the result is written with it redacted.

import { randomBytes } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import { setTimeout as sleep } from 'node:timers/promises';
import { chromium, devices } from 'playwright-core';
import {
  CAPS,
  LIMITS,
  RefusedCookies,
  SIGN_IN_REASONS,
  abortedByNavigation,
  blameBlockedNavigation,
  blockedByReadOnly,
  blockedRequestEntry,
  buildResult,
  errorResult,
  firstPartyFailure,
  hasVisiblePasswordField,
  isAllowedUrl,
  isReadOnlyMethod,
  normalizeInput,
  oneLine,
  pickSignInFields,
  pushCapped,
  recordFailedRequest,
  redactSecret,
  setSecret,
  signInPlan,
  staysAfterSignIn,
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
// The sign-in (signIn): how long to wait for its form to appear, for an email
// field alone to be joined by a password field before acting on it, for the
// password field of an email-first form, before clicking the form's submit
// control, and for the form to go away.
const SIGN_IN_FORM_WAIT_MS = 10000;
const SIGN_IN_EMAIL_ONLY_SETTLE_MS = 1000;
const SIGN_IN_PASSWORD_WAIT_MS = 5000;
const SIGN_IN_CLICK_AFTER_MS = 2000;
const SIGN_IN_SUBMITTED_WAIT_MS = 10000;
const SIGN_IN_POLL_MS = 200;
const SIGN_IN_FIELD_TIMEOUT_MS = 5000;

// OUTLINE_RAW_MAX bounds how many visible elements readOutline hands back;
// lib.mjs's buildOutline keeps CAPS.outlineEntries of them.
const OUTLINE_RAW_MAX = 200;
const POPUP_BINDING = '__verifyBrowserBlockedPopup';
const READ_ONLY_BINDING = '__verifyBrowserBlockedRequest';

async function main() {
  const [inputPath, resultPath, proxyAddr] = process.argv.slice(2);
  if (!inputPath || !resultPath || !proxyAddr) {
    throw new Error('usage: node run.mjs <input.json> <result path> <proxy host:port>');
  }
  const started = Date.now();
  const c = newCollector();
  let result;
  try {
    const input = normalizeInput(parseJSON(await readFile(inputPath, 'utf8')));
    setSecret(input.signIn?.password ?? '');
    const wrapUp = Math.min(MAX_WRAP_UP_MS, Math.floor(input.timeoutMs / 4));
    result = await check(input, proxyAddr, started + input.timeoutMs - wrapUp, c);
  } catch (err) {
    result = errorResult(errorText(err), c.blockedRequests);
  }
  await writeFile(resultPath, JSON.stringify(redactSecret(result)));
}

// parseJSON is JSON.parse with an error that quotes none of the text, which
// may hold sign_in's password.
function parseJSON(text) {
  try {
    return JSON.parse(text);
  } catch {
    throw new Error('input.json is not valid JSON');
  }
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
    // signIn is the sign-in's outcome (null without sign_in); signingIn is
    // true until it ended, and what the page does meanwhile is not the
    // check's. docStatus is the last main-frame document response's status.
    signIn: null,
    signingIn: false,
    docStatus: 0,
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

  if (input.signIn && !(await signIn(context, page, input, remaining, c))) {
    // The check's url is not opened; buildResult reports every step and
    // assertion as not run.
    c.steps = input.steps.map((s) => ({ action: s.action, ok: false }));
    c.assertions = input.assertions.map((a) => ({ type: a.type, passed: false }));
  } else {
    await loadAndCheck(page, input, remaining, c);
  }
  c.finalUrl = page.url();
  c.domText = await withTimeout(
    page.evaluate(() => (document.body ? document.body.innerText : '')),
    EVALUATE_TIMEOUT_MS,
    '',
  );
  c.outline = await withTimeout(page.evaluate(readOutline, OUTLINE_RAW_MAX), EVALUATE_TIMEOUT_MS, []);
  c.flushRefusedCookies?.();
  return buildResult(c);
}

// signIn signs in with sign_in before the check's first load and records the
// outcome in c.signIn: it opens the sign-in page, fills the app's own
// password form in the main frame and submits it. Its actions are not steps.
// From its start to the end of the check, the cookies the browser refuses for
// the Task's previews are collected (watchRefusedCookies). The password goes
// nowhere but the field it is typed into: a failure is one of
// SIGN_IN_REASONS, never a Playwright error, which could quote it.
async function signIn(context, page, input, remaining, c) {
  const cookies = new RefusedCookies(input.allowedHosts);
  c.signIn = { signedIn: false, reason: '', signedOutOnLoad: false, failedRequests: [], refusedCookies: cookies.entries };
  c.flushRefusedCookies = () => cookies.flush();
  await watchRefusedCookies(context, page, cookies);
  c.signingIn = true;
  try {
    const reason = await attemptSignIn(page, input.signIn, remaining);
    c.signIn.signedIn = reason === '';
    c.signIn.reason = reason;
  } finally {
    c.signingIn = false;
  }
  return c.signIn.signedIn;
}

// attemptSignIn returns '' when it signed in, else the reason it didn't.
async function attemptSignIn(page, creds, remaining) {
  try {
    await page.goto(creds.url, {
      waitUntil: 'domcontentloaded',
      timeout: Math.max(1, Math.min(FIRST_LOAD_TIMEOUT_MS, remaining())),
    });
  } catch (err) {
    return SIGN_IN_REASONS.loadFailed + errorText(err);
  }
  await waitQuietly(page, 'load', LOAD_EVENT_WAIT_MS, remaining);
  await waitQuietly(page, 'networkidle', NETWORK_IDLE_WAIT_MS, remaining);
  let fields = await waitForSignInForm(page, remaining);
  const { plan, reason } = signInPlan(fields);
  if (plan === 'fail') {
    return reason;
  }
  if (!(await fillInput(page, fields.email, creds.email, remaining))) {
    return SIGN_IN_REASONS.noEmailField;
  }
  if (plan === 'email-first') {
    await pressEnter(page, fields.email, remaining);
    fields = await waitForPasswordField(page, remaining);
    if (fields.password < 0) {
      return SIGN_IN_REASONS.noPasswordField;
    }
  }
  // Picked again: filling the email may have added or removed inputs.
  const password = await passwordField(page);
  if (!password) {
    return SIGN_IN_REASONS.noPasswordField;
  }
  try {
    if (!(await fillHandle(password, creds.password, remaining))) {
      return SIGN_IN_REASONS.noPasswordField;
    }
    return await submitSignIn(page, password, remaining);
  } finally {
    await password.dispose().catch(() => {});
  }
}

// passwordField is a handle on the password field pickSignInFields picks in
// the page as it is now, once the element is confirmed to be a password
// input, or null.
async function passwordField(page) {
  const inputs = await readInputs(page);
  const index = inputs === null ? -1 : pickSignInFields(inputs).password;
  if (index < 0) {
    return null;
  }
  const handle = await withTimeout(signInInputs(page).nth(index).elementHandle({ timeout: EVALUATE_TIMEOUT_MS }), EVALUATE_TIMEOUT_MS, null);
  if (!handle) {
    return null;
  }
  const isPassword = await withTimeout(
    handle.evaluate((el) => (el.getAttribute('type') ?? '').trim().toLowerCase() === 'password'),
    EVALUATE_TIMEOUT_MS,
    false,
  );
  if (!isPassword) {
    await handle.dispose().catch(() => {});
    return null;
  }
  return handle;
}

// fillHandle types value into the element and reports whether it could. The
// error is dropped: it may quote the value.
async function fillHandle(handle, value, remaining) {
  const timeout = Math.min(SIGN_IN_FIELD_TIMEOUT_MS, remaining());
  if (timeout <= 0) {
    return false;
  }
  return handle.fill(value, { timeout }).then(
    () => true,
    () => false,
  );
}

// waitForSignInForm waits for the sign-in form a client-side app renders
// after load: until there is a password field, or an email field alone has
// stayed alone for a moment (a form that asks for the email first), or
// SIGN_IN_FORM_WAIT_MS passed. It returns pickSignInFields' choice.
async function waitForSignInForm(page, remaining) {
  const start = Date.now();
  let emailAloneSince = 0;
  for (;;) {
    const fields = pickSignInFields(await readInputs(page));
    if (fields.password >= 0) {
      return fields;
    }
    emailAloneSince = fields.email >= 0 ? emailAloneSince || Date.now() : 0;
    if (emailAloneSince && Date.now() - emailAloneSince >= SIGN_IN_EMAIL_ONLY_SETTLE_MS) {
      return fields;
    }
    if (Date.now() - start >= SIGN_IN_FORM_WAIT_MS || remaining() <= SIGN_IN_POLL_MS) {
      return fields;
    }
    await sleep(SIGN_IN_POLL_MS);
  }
}

// waitForPasswordField waits up to SIGN_IN_PASSWORD_WAIT_MS for the password
// field an email-first form shows once the email was entered.
async function waitForPasswordField(page, remaining) {
  const start = Date.now();
  for (;;) {
    const fields = pickSignInFields(await readInputs(page));
    if (fields.password >= 0 || Date.now() - start >= SIGN_IN_PASSWORD_WAIT_MS || remaining() <= SIGN_IN_POLL_MS) {
      return fields;
    }
    await sleep(SIGN_IN_POLL_MS);
  }
}

// submitSignIn presses Enter in the password field; when the field is still
// there and enabled SIGN_IN_CLICK_AFTER_MS later, it clicks its form's submit
// control once. password is a handle on the field. It returns '' once no password field is visible (seen twice,
// either side of the page's load event, so a page between documents doesn't
// count), or the reason after SIGN_IN_SUBMITTED_WAIT_MS.
async function submitSignIn(page, password, remaining) {
  const submitted = Date.now();
  const timeout = Math.min(SIGN_IN_FIELD_TIMEOUT_MS, remaining());
  if (timeout > 0) {
    await password.press('Enter', { timeout }).catch(() => {});
  }
  let clicked = false;
  for (;;) {
    const elapsed = Date.now() - submitted;
    const inputs = await readInputs(page);
    if (inputs !== null && !hasVisiblePasswordField(inputs)) {
      await waitQuietly(page, 'load', Math.max(0, SIGN_IN_SUBMITTED_WAIT_MS - elapsed), remaining);
      const again = await readInputs(page);
      if (again !== null && !hasVisiblePasswordField(again)) {
        return '';
      }
    }
    if (elapsed >= SIGN_IN_SUBMITTED_WAIT_MS || remaining() <= 0) {
      return SIGN_IN_REASONS.stillOnForm;
    }
    if (!clicked && inputs !== null && elapsed >= SIGN_IN_CLICK_AFTER_MS) {
      clicked = true;
      await clickSubmitControl(page, inputs, remaining);
    }
    await sleep(Math.max(0, Math.min(SIGN_IN_POLL_MS, remaining())));
  }
}

// signInInputs are the inputs of the main frame (open shadow roots included,
// not iframes); readInputs describes them in the same order, so a
// pickSignInFields index is an nth() of it.
function signInInputs(page) {
  return page.locator('input');
}

// readInputs describes the main frame's inputs for pickSignInFields, or
// returns null when the page couldn't be read (between documents).
async function readInputs(page) {
  return withTimeout(signInInputs(page).evaluateAll(describeInputs), EVALUATE_TIMEOUT_MS, null);
}

// describeInputs runs in the page.
function describeInputs(inputs) {
  return inputs.map((el) => {
    const r = el.getBoundingClientRect();
    const shown =
      typeof el.checkVisibility === 'function'
        ? el.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true })
        : el.getClientRects().length > 0;
    return {
      type: el.getAttribute('type') || '',
      autocomplete: el.getAttribute('autocomplete') || '',
      name: el.getAttribute('name') || '',
      id: el.getAttribute('id') || '',
      visible: shown && r.width > 0 && r.height > 0,
      // :disabled, unlike el.disabled, counts a disabled ancestor fieldset.
      enabled: !el.matches(':disabled'),
    };
  });
}

// fillInput types value into the index-th input and reports whether it
// could. The error is dropped: it may quote the value.
async function fillInput(page, index, value, remaining) {
  const timeout = Math.min(SIGN_IN_FIELD_TIMEOUT_MS, remaining());
  if (timeout <= 0) {
    return false;
  }
  return signInInputs(page)
    .nth(index)
    .fill(value, { timeout })
    .then(
      () => true,
      () => false,
    );
}

async function pressEnter(page, index, remaining) {
  const timeout = Math.min(SIGN_IN_FIELD_TIMEOUT_MS, remaining());
  if (timeout > 0) {
    await signInInputs(page).nth(index).press('Enter', { timeout }).catch(() => {});
  }
}

// clickSubmitControl clicks the visible submit control of the password
// field's form, if the field is still visible and enabled and has one.
async function clickSubmitControl(page, inputs, remaining) {
  const { password } = pickSignInFields(inputs);
  const timeout = Math.min(SIGN_IN_FIELD_TIMEOUT_MS, remaining());
  if (password < 0 || timeout <= 0) {
    return;
  }
  let handle = null;
  try {
    handle = await signInInputs(page).nth(password).evaluateHandle(findSubmitControl, undefined, { timeout });
    const control = handle.asElement();
    if (control) {
      await control.click({ timeout });
    }
  } catch {
    // Not clicked; the form is reported as still showing.
  } finally {
    await handle?.dispose().catch(() => {});
  }
}

// findSubmitControl runs in the page: the first visible submit control of
// the input's form — a button without a type, type="submit", or an input
// type="submit" — or null.
function findSubmitControl(input) {
  const form = input.form;
  if (!form) {
    return null;
  }
  for (const el of form.elements) {
    const tag = el.tagName.toLowerCase();
    const type = (el.getAttribute('type') ?? '').trim().toLowerCase();
    const submits = (tag === 'button' && (el.getAttribute('type') === null || type === 'submit')) || (tag === 'input' && type === 'submit');
    if (!submits) {
      continue;
    }
    const r = el.getBoundingClientRect();
    const shown =
      typeof el.checkVisibility === 'function'
        ? el.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true })
        : el.getClientRects().length > 0;
    if (shown && r.width > 0 && r.height > 0) {
      return el;
    }
  }
  return null;
}

// watchRefusedCookies collects the cookies the browser refuses to store or to
// send for the page's requests, from the DevTools Network domain of a
// session of its own (only a check with sign_in opens it).
async function watchRefusedCookies(context, page, cookies) {
  const cdp = await context.newCDPSession(page);
  cdp.on('Network.requestWillBeSent', (ev) =>
    cookies.request(ev.requestId, ev.request?.url, ev.redirectResponse ? ev.redirectHasExtraInfo : undefined),
  );
  cdp.on('Network.responseReceived', (ev) => cookies.response(ev.requestId, ev.hasExtraInfo));
  cdp.on('Network.requestWillBeSentExtraInfo', (ev) => cookies.refused(ev.requestId, 'request', ev));
  cdp.on('Network.responseReceivedExtraInfo', (ev) => cookies.refused(ev.requestId, 'response', ev));
  await cdp.send('Network.enable');
}

// loadAndCheck is the check after the browser is ready (and signed in): the
// first load, the steps and the assertions. Signed in, a check of the front
// page of the host the app landed on stays on the page the app showed instead
// of loading it again.
async function loadAndCheck(page, input, remaining, c) {
  const blockedBefore = c.blockedCount;
  let loaded;
  if (c.signIn && staysAfterSignIn(input.url, page.url())) {
    loaded = await settleAfterSignIn(page, remaining, c);
  } else {
    loaded = await firstLoad(page, input.url, remaining, c);
    if (loaded && c.signIn) {
      c.signIn.signedOutOnLoad = hasVisiblePasswordField(await readInputs(page));
    }
  }
  if (!loaded && c.blockedCount === blockedBefore) {
    throw new Error(`the page could not be loaded: ${c.firstLoadError}`);
  }
  await runSteps(page, input.steps, remaining, c, loaded);
  for (const a of input.assertions) {
    c.assertions.push({ type: a.type, ...(await checkAssertion(page, a, remaining)) });
  }
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

// guardReadOnly holds a read_only check to its rule where guardNavigations
// (the page target's own DevTools Fetch interception) cannot see: a context
// route fails every request with another method that any frame —
// out-of-process ones included — or dedicated worker sends (Playwright
// intercepts each of their targets before letting it run). Request
// interception is enforced by the browser, so no page script can get around
// it.
//
// What no request interception sees is refused instead, in every frame,
// before the page's own scripts run (readOnlyGuard): WebSocket and
// WebSocketStream, SharedWorker (its requests reach no route), WebTransport
// and RTCPeerConnection.
//
// THE THREAT MODEL is the site's own code doing what it normally does while
// QA clicks, types and navigates — a sign-up form submitted, a cart updated,
// an analytics beacon posted. It is not a sandbox against code written to get
// around it; the QA agent can't run scripts, only steps.
//
// Each refusal is reported to onBlocked through a binding that only accepts
// calls carrying this check's random secret, which only the guard holds, so
// the page can't add entries to blocked_requests.
async function guardReadOnly(context, onBlocked) {
  const secret = randomBytes(16).toString('hex');
  await context.exposeBinding(READ_ONLY_BINDING, (_source, key, url, method) => {
    if (key === secret) {
      onBlocked(String(url), String(method));
    }
  });
  await context.addInitScript(readOnlyGuard, { binding: READ_ONLY_BINDING, secret });
  await context.route('**/*', (route) => {
    const req = route.request();
    if (!isReadOnlyMethod(req.method())) {
      onBlocked(req.url(), req.method());
      return route.abort('aborted');
    }
    return route.fallback();
  });
}

// readOnlyGuard runs before any of the page's own scripts in every frame of a
// read_only check. It refuses the channels no request interception sees,
// reporting each attempt with the method it is named by:
//   - WebSocket ("WEBSOCKET"): the socket fails as for a server that never
//     answered (error, then close with code 1006), so the page carries on;
//     WebSocketStream throws;
//   - SharedWorker ("SHAREDWORKER"), WebTransport ("WEBTRANSPORT") and
//     RTCPeerConnection ("WEBRTC"): the constructor throws a SecurityError.
// Each real constructor's prototype.constructor points at the refusing one, so
// an existing prototype doesn't lead back to the real constructor.
function readOnlyGuard(o) {
  const g = globalThis;
  // Taken now, before any page script can replace it, so the secret only
  // ever reaches the real binding.
  const call = g[o.binding];
  // A script-created about:blank frame resolves against its parent's URL
  // (document.baseURI), as the browser itself would.
  const base = () => (document.baseURI && document.baseURI !== 'about:blank' ? document.baseURI : location.href);
  const report = (u, method) => {
    // A channel opened without a URL (RTCPeerConnection) is reported as the
    // page that opened it.
    let url = u === undefined || u === null ? base() : String(u);
    try {
      url = new URL(url, base()).href;
    } catch {
      // Reported as given.
    }
    try {
      call(o.secret, url, method);
    } catch {
      // Best-effort: the channel is refused either way.
    }
  };
  const replace = (name, refusing) => {
    const Real = g[name];
    if (typeof Real !== 'function') {
      return;
    }
    Object.defineProperty(refusing, 'name', { value: name });
    refusing.prototype = Real.prototype;
    for (const k of Object.getOwnPropertyNames(Real)) {
      if (/^[A-Z_]+$/.test(k)) {
        try {
          Object.defineProperty(refusing, k, { value: Real[k] });
        } catch {
          // A constant that can't be copied is left out.
        }
      }
    }
    try {
      Object.defineProperty(Real.prototype, 'constructor', { value: refusing, writable: true, configurable: true });
    } catch {
      // A prototype that can't be changed keeps its constructor.
    }
    try {
      Object.defineProperty(g, name, { value: refusing, writable: true, configurable: true });
    } catch {
      // A property that can't be redefined keeps its value.
    }
  };
  const refuse = (name, method) =>
    replace(name, function (u) {
      report(u, method);
      throw new DOMException(`${name} is blocked in a read-only check`, 'SecurityError');
    });
  replace('WebSocket', function (u) {
    if (!new.target) {
      throw new TypeError("Failed to construct 'WebSocket': Please use the 'new' operator");
    }
    report(u, 'WEBSOCKET');
    const ws = new EventTarget();
    let url = String(u);
    try {
      url = new URL(url, base()).href;
    } catch {
      // Kept as given.
    }
    Object.defineProperties(ws, {
      url: { value: url },
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
  });
  refuse('WebSocketStream', 'WEBSOCKET');
  refuse('SharedWorker', 'SHAREDWORKER');
  refuse('WebTransport', 'WEBTRANSPORT');
  refuse('RTCPeerConnection', 'WEBRTC');
  refuse('webkitRTCPeerConnection', 'WEBRTC');
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
//
// With sign_in, what happens before the sign-in ended is not the check's: a
// request that started then goes to sign_in.failed_requests, and page and
// console errors from then are dropped.
function watchPage(page, allowedHosts, allowed, c, readOnly) {
  let mainNavigations = 0;
  const frameNavigations = new WeakMap();
  const counts = (frame) => ({ main: mainNavigations, frame: frameNavigations.get(frame) ?? 0 });
  const sentAt = new WeakMap();
  const duringSignIn = new WeakMap();
  const signInRequest = (req) => duringSignIn.get(req) ?? c.signingIn;
  page.on('request', (req) => {
    duringSignIn.set(req, c.signingIn);
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
    if (c.signingIn) {
      return;
    }
    const text = err?.name && err?.message ? `${err.name}: ${err.message}` : String(err?.message ?? err);
    pushCapped(c.pageErrors, text, CAPS.pageErrors);
  });
  page.on('console', (msg) => {
    if (msg.type() === 'error' && !c.signingIn) {
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
    recordFailedRequest(c, entry, signInRequest(req));
  });
  page.on('response', (resp) => {
    const req = resp.request();
    if (req.isNavigationRequest() && resp.frame() === page.mainFrame()) {
      c.docStatus = resp.status();
    }
    const entry = firstPartyFailure({ url: resp.url(), method: req.method(), status: resp.status(), failure: '' }, allowedHosts);
    recordFailedRequest(c, entry, signInRequest(req));
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

// settleAfterSignIn is the first load of a check that stays on the page the
// app showed after signing in: it waits for that page to settle as firstLoad
// does and reads its timings. http_status is the last main-frame document
// response's since the sign-in started.
async function settleAfterSignIn(page, remaining, c) {
  await waitQuietly(page, 'load', LOAD_EVENT_WAIT_MS, remaining);
  await waitQuietly(page, 'networkidle', NETWORK_IDLE_WAIT_MS, remaining);
  c.timings = await withTimeout(page.evaluate(readTimings), EVALUATE_TIMEOUT_MS, {});
  c.httpStatus = c.docStatus;
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
      case 'back': {
        // goBack returns null for a client-side route change (pushState), so
        // null alone is fine; it went nowhere if the URL didn't change, or it
        // landed on the tab's initial blank page.
        const from = page.url();
        const response = await page.goBack({ waitUntil: 'domcontentloaded', timeout });
        if ((response === null && page.url() === from) || page.url() === 'about:blank') {
          return 'there is no earlier page in this check to go back to';
        }
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

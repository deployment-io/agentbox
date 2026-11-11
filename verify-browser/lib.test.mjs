import assert from 'node:assert/strict';
import { afterEach, describe, it } from 'node:test';
import {
  CAPS,
  LIMITS,
  READ_ONLY_METHODS,
  RefusedCookies,
  SIGN_IN_NOT_RUN,
  SIGN_IN_REASONS,
  abortedByNavigation,
  blameBlockedNavigation,
  blockedByReadOnly,
  blockedRequestEntry,
  buildOutline,
  buildResult,
  capString,
  computeVerdict,
  errorResult,
  firstPartyFailure,
  hasVisiblePasswordField,
  isAllowedUrl,
  isReadOnlyMethod,
  normalizeInput,
  oneLine,
  normalizeViewport,
  pickSignInFields,
  pushCapped,
  recordFailedRequest,
  redactSecret,
  setSecret,
  resultViewport,
  signInPlan,
  staysAfterSignIn,
  viewportContextOptions,
} from './lib.mjs';

const HOSTS = ['preview-abc.example.net'];

function validInput(overrides = {}) {
  return {
    schema_version: 1,
    url: 'https://preview-abc.example.net/',
    allowed_hosts: HOSTS,
    steps: [],
    assertions: [],
    timeout_ms: 30000,
    ...overrides,
  };
}

function passing(overrides = {}) {
  return {
    http_status: 200,
    steps: [{ ok: true }],
    assertions: [{ passed: true }],
    page_errors: [],
    console_errors: [],
    failed_requests: [],
    blocked_navigations: [],
    ...overrides,
  };
}

describe('isAllowedUrl', () => {
  it('accepts https on an allowed host with no port or port 443', () => {
    assert.equal(isAllowedUrl('https://preview-abc.example.net/a?b=c', HOSTS), true);
    assert.equal(isAllowedUrl('https://preview-abc.example.net:443/', HOSTS), true);
    assert.equal(isAllowedUrl('https://PREVIEW-ABC.example.net./', HOSTS), true);
  });
  it('requires an exact host match', () => {
    assert.equal(isAllowedUrl('https://evil.preview-abc.example.net/', HOSTS), false);
    assert.equal(isAllowedUrl('https://preview-abc.example.net.evil.com/', HOSTS), false);
    assert.equal(isAllowedUrl('https://example.net/', HOSTS), false);
  });
  it('refuses other schemes, ports, user info and junk', () => {
    assert.equal(isAllowedUrl('http://preview-abc.example.net/', HOSTS), false);
    assert.equal(isAllowedUrl('https://preview-abc.example.net:8443/', HOSTS), false);
    assert.equal(isAllowedUrl('https://user:pw@preview-abc.example.net/', HOSTS), false);
    assert.equal(isAllowedUrl('data:text/html,hi', HOSTS), false);
    assert.equal(isAllowedUrl('not a url', HOSTS), false);
    assert.equal(isAllowedUrl('https://169.254.169.254/', HOSTS), false);
  });
});

describe('normalizeInput', () => {
  it('accepts a valid input and normalises hosts', () => {
    const got = normalizeInput(
      validInput({
        allowed_hosts: ['Preview-ABC.example.net.'],
        steps: [
          { action: 'goto', url: 'https://preview-abc.example.net/about' },
          { action: 'click', selector: 'text=More' },
          { action: 'fill', selector: '#q', value: '' },
          { action: 'press', selector: '#q', key: 'Enter' },
          { action: 'wait_for', text: 'Results' },
        ],
        assertions: [
          { type: 'text_visible', text: 'Results' },
          { type: 'url_path', path: '/about' },
        ],
      }),
    );
    assert.deepEqual(got.allowedHosts, ['preview-abc.example.net']);
    assert.equal(got.steps.length, 5);
    assert.equal(got.timeoutMs, 30000);
  });
  it('accepts a back step and drops its unused fields', () => {
    const got = normalizeInput(validInput({ steps: [{ action: 'back', selector: 'a' }] }));
    assert.deepEqual(got.steps, [{ action: 'back' }]);
  });
  const bad = [
    ['schema_version', { schema_version: 2 }],
    ['non-https url', { url: 'http://preview-abc.example.net/' }],
    ['url outside allowed_hosts', { url: 'https://other.example.net/' }],
    ['goto outside allowed_hosts', { steps: [{ action: 'goto', url: 'https://www.iana.org/' }] }],
    ['unknown action', { steps: [{ action: 'hover', selector: 'a' }] }],
    ['long unused back field', { steps: [{ action: 'back', url: 'u'.repeat(LIMITS.maxStringChars + 1) }] }],
    ['wait_for with both', { steps: [{ action: 'wait_for', selector: 'a', text: 'b' }] }],
    ['unknown assertion', { assertions: [{ type: 'title_is', text: 'x' }] }],
    ['relative url_path', { assertions: [{ type: 'url_path', path: 'about' }] }],
    ['too many steps', { steps: Array(LIMITS.maxSteps + 1).fill({ action: 'click', selector: 'a' }) }],
    ['too many assertions', { assertions: Array(LIMITS.maxAssertions + 1).fill({ type: 'text_visible', text: 'a' }) }],
    ['long string', { steps: [{ action: 'click', selector: 'a'.repeat(LIMITS.maxStringChars + 1) }] }],
    ['long unused step field', { steps: [{ action: 'click', selector: 'a', value: 'v'.repeat(LIMITS.maxStringChars + 1) }] }],
    [
      'long unused assertion field',
      { assertions: [{ type: 'text_visible', text: 'a', selector: 's'.repeat(LIMITS.maxStringChars + 1) }] },
    ],
    ['timeout too short', { timeout_ms: 4999 }],
    ['timeout too long', { timeout_ms: 90001 }],
    ['no allowed hosts', { allowed_hosts: [] }],
  ];
  for (const [name, overrides] of bad) {
    it(`rejects ${name}`, () => {
      assert.throws(() => normalizeInput(validInput(overrides)));
    });
  }
});

describe('read_only', () => {
  it('defaults to false', () => {
    assert.equal(normalizeInput(validInput()).readOnly, false);
  });
  it('accepts true and false', () => {
    assert.equal(normalizeInput(validInput({ read_only: true })).readOnly, true);
    assert.equal(normalizeInput(validInput({ read_only: false })).readOnly, false);
  });
  for (const v of [null, 'true', 1, 0, {}, []]) {
    it(`rejects ${JSON.stringify(v)}`, () => {
      assert.throws(() => normalizeInput(validInput({ read_only: v })), /read_only/);
    });
  }
  it('lets GET, HEAD and OPTIONS through, in any case', () => {
    assert.deepEqual([...READ_ONLY_METHODS], ['GET', 'HEAD', 'OPTIONS']);
    for (const m of ['GET', 'HEAD', 'OPTIONS', 'get', 'Head', 'options', 'gEt']) {
      assert.equal(isReadOnlyMethod(m), true, m);
    }
  });
  it('blocks every other method, in any case', () => {
    for (const m of ['POST', 'PUT', 'PATCH', 'DELETE', 'post', 'Put', 'patch', 'delete', 'CONNECT', 'TRACE', 'PROPFIND', 'GETX', '']) {
      assert.equal(isReadOnlyMethod(m), false, m);
    }
    for (const m of [undefined, null, 1, {}]) {
      assert.equal(isReadOnlyMethod(m), false, String(m));
    }
  });
  it('keeps blocked requests out of failed_requests only when read_only', () => {
    assert.equal(blockedByReadOnly('POST', true), true);
    assert.equal(blockedByReadOnly('delete', true), true);
    assert.equal(blockedByReadOnly('GET', true), false);
    assert.equal(blockedByReadOnly('POST', false), false);
    assert.equal(blockedByReadOnly('POST', undefined), false);
  });
  it('builds blocked_requests in order, capped, always present', () => {
    assert.deepEqual(buildResult({}).blocked_requests, []);
    const entries = Array.from({ length: CAPS.blockedRequests + 5 }, (_, i) =>
      blockedRequestEntry(`https://preview-abc.example.net/${i}`, 'POST'),
    );
    const r = buildResult({ httpStatus: 200, steps: [], assertions: [], blockedRequests: entries });
    assert.equal(CAPS.blockedRequests, 20);
    assert.equal(r.blocked_requests.length, CAPS.blockedRequests);
    assert.deepEqual(r.blocked_requests[0], { url: 'https://preview-abc.example.net/0', method: 'POST' });
    assert.deepEqual(r.blocked_requests[19], { url: 'https://preview-abc.example.net/19', method: 'POST' });
    assert.equal(r.verdict, 'pass', 'a blocked request decides nothing on its own');
  });
  it('caps a blocked request URL', () => {
    const e = blockedRequestEntry('https://x/' + 'p'.repeat(5000), 'POST');
    assert.equal([...e.url].length, CAPS.urlChars);
  });
});

describe('viewport', () => {
  // DEVICES mirrors playwright-core 1.64.0's descriptors for the presets.
  const DEVICES = {
    'Pixel 7': {
      userAgent: 'Mozilla/5.0 (Linux; Android 14; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.8078.4 Mobile Safari/537.36',
      viewport: { width: 412, height: 839 },
      screen: { width: 412, height: 915 },
      deviceScaleFactor: 2.625,
      isMobile: true,
      hasTouch: true,
      defaultBrowserType: 'chromium',
    },
    'Galaxy Tab S4': {
      userAgent: 'Mozilla/5.0 (Linux; Android 8.1.0; SM-T837A) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.8078.4 Safari/537.36',
      viewport: { width: 712, height: 1138 },
      deviceScaleFactor: 2.25,
      isMobile: true,
      hasTouch: true,
      defaultBrowserType: 'chromium',
    },
  };
  const options = (raw) => viewportContextOptions(normalizeViewport(raw), DEVICES);

  it('defaults to desktop when absent, leaving the rest of the input as it was', () => {
    const got = normalizeInput(validInput());
    assert.deepEqual(got.viewport, { preset: 'desktop', width: 1280, height: 720, mobile: false, device: '' });
    const { viewport, ...rest } = got;
    assert.deepEqual(rest, {
      url: 'https://preview-abc.example.net/',
      allowedHosts: HOSTS,
      steps: [],
      assertions: [],
      timeoutMs: 30000,
      readOnly: false,
      signIn: null,
    });
    assert.deepEqual(options(undefined), {
      viewport: { width: 1280, height: 720 },
      deviceScaleFactor: 1,
      isMobile: false,
      hasTouch: false,
    });
  });
  it('desktop preset is 1280×720 without emulation', () => {
    assert.deepEqual(options({ preset: 'desktop' }), options(undefined));
  });
  it('mobile preset is the Pixel 7 descriptor', () => {
    const o = options({ preset: 'mobile' });
    assert.deepEqual(o.viewport, { width: 412, height: 839 });
    assert.deepEqual(o.screen, { width: 412, height: 915 });
    assert.equal(o.deviceScaleFactor, 2.625);
    assert.equal(o.isMobile, true);
    assert.equal(o.hasTouch, true);
    assert.match(o.userAgent, /Android .*Pixel 7.*Chrome/);
    assert.equal('defaultBrowserType' in o, false);
  });
  it('tablet preset is the Galaxy Tab S4 descriptor', () => {
    const o = options({ preset: 'tablet' });
    assert.deepEqual(o.viewport, { width: 712, height: 1138 });
    assert.equal(o.deviceScaleFactor, 2.25);
    assert.equal(o.isMobile, true);
    assert.equal(o.hasTouch, true);
    assert.match(o.userAgent, /Android .*SM-T837A.*Chrome/);
    assert.equal('defaultBrowserType' in o, false);
  });
  it('a preset whose descriptor changed size is an error', () => {
    const changed = { ...DEVICES, 'Pixel 7': { ...DEVICES['Pixel 7'], viewport: { width: 400, height: 800 } } };
    assert.throws(() => viewportContextOptions(normalizeViewport({ preset: 'mobile' }), changed), /Pixel 7/);
    assert.throws(() => viewportContextOptions(normalizeViewport({ preset: 'tablet' }), {}), /Galaxy Tab S4/);
  });
  it('a custom size has no emulation', () => {
    assert.deepEqual(options({ width: 1279, height: 800 }), {
      viewport: { width: 1279, height: 800 },
      deviceScaleFactor: 1,
      isMobile: false,
      hasTouch: false,
    });
  });
  for (const [w, h] of [
    [320, 480],
    [2560, 1600],
    [320, 1600],
    [2560, 480],
  ]) {
    it(`accepts ${w}×${h}`, () => {
      assert.deepEqual(normalizeViewport({ width: w, height: h }), { preset: '', width: w, height: h, mobile: false, device: '' });
    });
  }
  const bad = [
    ['width 319', { width: 319, height: 600 }, /viewport\.width/],
    ['width 2561', { width: 2561, height: 600 }, /viewport\.width/],
    ['height 479', { width: 800, height: 479 }, /viewport\.height/],
    ['height 1601', { width: 800, height: 1601 }, /viewport\.height/],
    ['a fractional width', { width: 800.5, height: 600 }, /viewport\.width/],
    ['a string height', { width: 800, height: '600' }, /viewport\.height/],
    ['an unknown preset', { preset: 'watch' }, /viewport\.preset/],
    ['a non-string preset', { preset: 1 }, /viewport\.preset/],
    ['both forms', { preset: 'mobile', width: 800, height: 600 }, /not both/],
    ['preset and width', { preset: 'mobile', width: 800 }, /not both/],
    ['width alone', { width: 800 }, /viewport/],
    ['an unknown key', { preset: 'mobile', orientation: 'landscape' }, /viewport/],
    ['an empty object', {}, /viewport/],
    ['null', null, /viewport/],
    ['a string', 'mobile', /viewport/],
    ['an array', [412, 839], /viewport/],
  ];
  for (const [name, viewport, want] of bad) {
    it(`rejects ${name}`, () => {
      assert.throws(() => normalizeInput(validInput({ viewport })), want);
    });
  }
  it('the result reports a preset with its size', () => {
    assert.deepEqual(resultViewport(normalizeViewport({ preset: 'mobile' })), {
      width: 412,
      height: 839,
      preset: 'mobile',
      mobile: true,
    });
    assert.deepEqual(resultViewport(normalizeViewport({ preset: 'tablet' })), {
      width: 712,
      height: 1138,
      preset: 'tablet',
      mobile: true,
    });
  });
  it('the result reports a custom size without a preset', () => {
    const r = buildResult({ httpStatus: 200, viewport: normalizeViewport({ width: 1279, height: 720 }) });
    assert.deepEqual(r.viewport, { width: 1279, height: 720, mobile: false });
  });
  it('the result reports the desktop default', () => {
    const want = { width: 1280, height: 720, preset: 'desktop', mobile: false };
    assert.deepEqual(buildResult({ httpStatus: 200, viewport: normalizeInput(validInput()).viewport }).viewport, want);
    assert.deepEqual(buildResult({ httpStatus: 200 }).viewport, want);
  });
});

describe('computeVerdict', () => {
  it('passes when every rule holds', () => {
    assert.equal(computeVerdict(passing()), 'pass');
    assert.equal(computeVerdict(passing({ steps: [], assertions: [] })), 'pass');
  });
  it('fails on a non-2xx first load', () => {
    assert.equal(computeVerdict(passing({ http_status: 404 })), 'fail');
    assert.equal(computeVerdict(passing({ http_status: 301 })), 'fail');
    assert.equal(computeVerdict(passing({ http_status: 0 })), 'fail');
  });
  it('fails on a failed step', () => {
    assert.equal(computeVerdict(passing({ steps: [{ ok: true }, { ok: false }] })), 'fail');
  });
  it('fails on a failed assertion', () => {
    assert.equal(computeVerdict(passing({ assertions: [{ passed: false }] })), 'fail');
  });
  it('fails on a page error', () => {
    assert.equal(computeVerdict(passing({ page_errors: ['TypeError: x is undefined'] })), 'fail');
  });
  it('fails on a failed request', () => {
    assert.equal(computeVerdict(passing({ failed_requests: [{ url: 'u', status: 500 }] })), 'fail');
  });
  it('ignores console errors and blocked navigations on their own', () => {
    assert.equal(computeVerdict(passing({ console_errors: ['boom'], blocked_navigations: ['https://x/'] })), 'pass');
  });
});

describe('firstPartyFailure', () => {
  it('reports an allowed host returning >= 400', () => {
    const got = firstPartyFailure({ url: 'https://preview-abc.example.net/api', method: 'POST', status: 500 }, HOSTS);
    assert.deepEqual(got, { url: 'https://preview-abc.example.net/api', method: 'POST', status: 500, failure: '' });
  });
  it('reports an allowed host request that failed', () => {
    const got = firstPartyFailure(
      { url: 'https://preview-abc.example.net/app.js', method: 'GET', status: 0, failure: 'net::ERR_CONNECTION_RESET' },
      HOSTS,
    );
    assert.equal(got.failure, 'net::ERR_CONNECTION_RESET');
  });
  it('ignores third-party hosts', () => {
    assert.equal(firstPartyFailure({ url: 'https://analytics.example.com/x', status: 500 }, HOSTS), null);
    assert.equal(firstPartyFailure({ url: 'https://cdn.example.com/x', failure: 'net::ERR_FAILED' }, HOSTS), null);
  });
  it('ignores successes and navigation cancellations', () => {
    assert.equal(firstPartyFailure({ url: 'https://preview-abc.example.net/', status: 304 }, HOSTS), null);
    assert.equal(
      firstPartyFailure(
        { url: 'https://preview-abc.example.net/a', status: 0, failure: 'net::ERR_ABORTED', abortedByNavigation: true },
        HOSTS,
      ),
      null,
    );
  });
  it('reports an abort that no navigation explains', () => {
    const got = firstPartyFailure(
      { url: 'https://preview-abc.example.net/api', method: 'GET', status: 0, failure: 'net::ERR_ABORTED' },
      HOSTS,
    );
    assert.equal(got.failure, 'net::ERR_ABORTED');
  });
});

describe('abortedByNavigation', () => {
  it('counts a main-frame navigation after the request was sent', () => {
    assert.equal(abortedByNavigation({ isNavigation: false, sent: { main: 1, frame: 0 }, now: { main: 2, frame: 0 } }), true);
  });
  it('does not excuse an abort with no navigation since', () => {
    assert.equal(abortedByNavigation({ isNavigation: false, sent: { main: 1, frame: 0 }, now: { main: 1, frame: 0 } }), false);
  });
  it('does not excuse a navigation request (an iframe document) nothing superseded', () => {
    assert.equal(abortedByNavigation({ isNavigation: true, sent: { main: 1, frame: 1 }, now: { main: 1, frame: 1 } }), false);
  });
  it('excuses a navigation superseded in its own frame', () => {
    assert.equal(abortedByNavigation({ isNavigation: true, sent: { main: 1, frame: 1 }, now: { main: 1, frame: 2 } }), true);
  });
  it('does not let a later navigation in another frame excuse a subresource', () => {
    assert.equal(abortedByNavigation({ isNavigation: false, sent: { main: 1, frame: 0 }, now: { main: 1, frame: 3 } }), false);
  });
});

describe('blameBlockedNavigation', () => {
  const step = (action, startDoc, endDoc, ok = true) => ({ action, ok, error: ok ? '' : 'x', startDoc, endDoc });
  it('fails every successful step that ran on the page the block happened on', () => {
    const steps = [step('goto', 0, 1), step('click', 1, 1), step('wait_for', 1, 1)];
    assert.equal(blameBlockedNavigation(steps, 1, 'https://elsewhere.example/'), 3);
    assert.ok(steps.every((s) => !s.ok && s.error.includes('https://elsewhere.example/')));
  });
  it('leaves steps that ran on an earlier page alone', () => {
    const steps = [step('click', 0, 0), step('goto', 0, 1), step('click', 1, 1)];
    blameBlockedNavigation(steps, 1, 'https://elsewhere.example/');
    assert.deepEqual(steps.map((s) => s.ok), [true, false, false]);
  });
  it('blames the last step when none ran on that page', () => {
    const steps = [step('click', 0, 0), step('click', 0, 0)];
    assert.equal(blameBlockedNavigation(steps, 2, 'https://elsewhere.example/'), 1);
    assert.deepEqual(steps.map((s) => s.ok), [true, false]);
  });
  it('keeps a failed step\'s own error', () => {
    const steps = [step('click', 0, 0, false)];
    assert.equal(blameBlockedNavigation(steps, 0, 'https://elsewhere.example/'), 0);
    assert.equal(steps[0].error, 'x');
  });
  it('makes the verdict fail', () => {
    const steps = [step('click', 0, 0), step('wait_for', 0, 0)];
    blameBlockedNavigation(steps, 0, 'https://elsewhere.example/');
    const r = buildResult({ httpStatus: 200, steps, assertions: [] });
    assert.equal(r.verdict, 'fail');
    assert.equal(r.steps[0].ok, false);
    assert.equal(Object.keys(r.steps[0]).sort().join(), 'action,error,index,ok');
  });
  it('does nothing without steps', () => {
    assert.equal(blameBlockedNavigation([], 0, 'https://elsewhere.example/'), 0);
  });
});

describe('caps', () => {
  const many = (n, s) => Array.from({ length: n }, (_, i) => `${s}${i}`);
  const result = buildResult({
    httpStatus: 200,
    finalUrl: 'https://preview-abc.example.net/' + 'p'.repeat(5000),
    steps: Array.from({ length: 2 }, () => ({ action: 'click', ok: false, error: 'e'.repeat(1000) })),
    assertions: [{ type: 'text_visible', passed: false, detail: 'd'.repeat(1000) }],
    pageErrors: many(15, 'x'.repeat(600)),
    consoleErrors: many(25, 'y'.repeat(400)),
    failedRequests: many(25, 'f').map((u) => ({ url: u, method: 'GET', status: 500, failure: '' })),
    blockedNavigations: many(12, 'https://out.example.com/'),
    domText: 'z'.repeat(2000),
    timings: { domContentLoaded: 12.4, load: 30.6 },
  });
  it('caps page errors', () => {
    assert.equal(result.page_errors.length, CAPS.pageErrors);
    assert.ok(result.page_errors.every((e) => [...e].length <= CAPS.pageErrorChars));
  });
  it('caps console errors', () => {
    assert.equal(result.console_errors.length, CAPS.consoleErrors);
    assert.ok(result.console_errors.every((e) => [...e].length <= CAPS.consoleErrorChars));
  });
  it('caps failed requests and blocked navigations', () => {
    assert.equal(result.failed_requests.length, CAPS.failedRequests);
    assert.equal(result.blocked_navigations.length, CAPS.blockedNavigations);
  });
  it('caps step errors, assertion details and the text snippet', () => {
    assert.ok(result.steps.every((s) => [...s.error].length <= CAPS.detailChars));
    assert.ok(result.assertions.every((a) => [...a.detail].length <= CAPS.detailChars));
    assert.equal([...result.dom_text_snippet].length, CAPS.domTextChars);
    assert.ok([...result.final_url].length <= CAPS.urlChars);
  });
  it('rounds timings and omits a missing lcp', () => {
    assert.deepEqual(result.timings_ms, { dom_content_loaded: 12, load: 31 });
  });
  it('indexes steps and assertions and decides the verdict', () => {
    assert.deepEqual(
      result.steps.map((s) => s.index),
      [0, 1],
    );
    assert.equal(result.verdict, 'fail');
    assert.equal(result.schema_version, 1);
    assert.equal(result.status, 'completed');
  });
  it('pushCapped stops at the cap and skips duplicates', () => {
    const list = [];
    for (const v of ['a', 'a', 'b', 'c']) {
      pushCapped(list, v, 2);
    }
    assert.deepEqual(list, ['a', 'b']);
  });
  it('capString is code-point safe', () => {
    assert.equal(capString('😀😀😀', 2), '😀…');
    assert.equal(capString('ok', 2), 'ok');
  });
  it('errorResult has no verdict', () => {
    const r = errorResult('boom\n  at x');
    assert.deepEqual(r, { schema_version: 1, status: 'error', error: 'boom at x', blocked_requests: [] });
  });
  it('errorResult keeps the requests read_only blocked before the check failed, capped', () => {
    const entries = Array.from({ length: CAPS.blockedRequests + 3 }, (_, i) =>
      blockedRequestEntry(`https://preview-abc.example.net/${i}`, 'POST'),
    );
    const r = errorResult('boom', entries);
    assert.equal(r.blocked_requests.length, CAPS.blockedRequests);
    assert.deepEqual(r.blocked_requests[0], { url: 'https://preview-abc.example.net/0', method: 'POST' });
  });
});

describe('outline', () => {
  const BASE = 'https://preview-abc.example.net/pricing/';

  it('classifies headings, links, buttons and fields in document order', () => {
    const outline = buildOutline(
      [
        { tag: 'h1', text: 'Pricing' },
        { tag: 'a', text: 'About us', href: '../about' },
        { tag: 'button', text: '  Sign\n up  ' },
        { tag: 'input', type: 'email', labelText: 'Email address', placeholder: 'you@example.com', name: 'email' },
        { tag: 'input', type: 'submit', value: 'Subscribe' },
        { tag: 'select', name: 'plan' },
        { tag: 'textarea', placeholder: 'Message' },
        { tag: 'div', text: 'not an outline element' },
      ],
      BASE,
    );
    assert.deepEqual(outline, [
      { kind: 'heading', text: 'Pricing', level: 1 },
      { kind: 'link', text: 'About us', href: 'https://preview-abc.example.net/about' },
      { kind: 'button', text: 'Sign up' },
      { kind: 'field', text: 'Email address' },
      { kind: 'button', text: 'Subscribe' },
      { kind: 'field', text: 'plan' },
      { kind: 'field', text: 'Message' },
    ]);
  });

  it('takes heading levels from the tag, aria-level, or the ARIA default', () => {
    const levels = buildOutline(
      [
        { tag: 'h3', text: 'a' },
        { tag: 'h2', text: 'b', level: 4 },
        { tag: 'div', role: 'heading', text: 'c', level: '5' },
        { tag: 'div', role: 'heading', text: 'd' },
        { tag: 'h6', text: 'e', level: 9 },
      ],
      BASE,
    ).map((e) => e.level);
    assert.deepEqual(levels, [3, 4, 5, 2, 6]);
  });

  it('resolves hrefs to absolute URLs and keeps only web, mailto: and tel: ones', () => {
    const outline = buildOutline(
      [
        { tag: 'a', text: 'Docs', href: '/docs?x=1#top' },
        { tag: 'a', text: 'Elsewhere', href: 'https://other.example.com/' },
        { tag: 'a', text: 'Mail', href: 'mailto:a@example.com' },
        { tag: 'a', text: 'Call', href: 'tel:+15551234' },
        { tag: 'a', text: 'JS', href: 'javascript:void(0)' },
        { tag: 'a', href: 'https://preview-abc.example.net/icon', ariaLabel: '' },
        { tag: 'a', href: 'javascript:void(0)' },
        { tag: 'a', text: 'Inline', href: 'data:text/html,<h1>x</h1>' },
        { tag: 'a', text: 'VB', href: 'vbscript:msgbox(1)' },
        { tag: 'a', text: 'no href' },
      ],
      BASE,
    );
    assert.deepEqual(outline, [
      { kind: 'link', text: 'Docs', href: 'https://preview-abc.example.net/docs?x=1#top' },
      { kind: 'link', text: 'Elsewhere', href: 'https://other.example.com/' },
      { kind: 'link', text: 'Mail', href: 'mailto:a@example.com' },
      { kind: 'link', text: 'Call', href: 'tel:+15551234' },
      { kind: 'link', text: 'JS' },
      { kind: 'link', text: '', href: 'https://preview-abc.example.net/icon' },
      { kind: 'link', text: 'Inline' },
      { kind: 'link', text: 'VB' },
    ]);
  });

  it('labels a field by label, then aria-label, placeholder, name', () => {
    const texts = buildOutline(
      [
        { tag: 'input', labelText: 'Label', ariaLabel: 'Aria', placeholder: 'Ph', name: 'n' },
        { tag: 'input', labelText: ' ', ariaLabel: 'Aria', placeholder: 'Ph', name: 'n' },
        { tag: 'input', placeholder: 'Ph', name: 'n' },
        { tag: 'input', name: 'n' },
        { tag: 'input' },
        { tag: 'div', role: 'textbox', ariaLabel: 'Editor' },
      ],
      BASE,
    ).map((e) => e.text);
    assert.deepEqual(texts, ['Label', 'Aria', 'Ph', 'n', 'Editor']);
  });

  it('gives value-less submit and reset inputs the browser default caption', () => {
    const texts = buildOutline(
      [
        { tag: 'input', type: 'submit' },
        { tag: 'input', type: 'reset', value: '' },
        { tag: 'input', type: 'submit', ariaLabel: 'Send' },
        { tag: 'input', type: 'button' },
      ],
      BASE,
    ).map((e) => e.text);
    assert.deepEqual(texts, ['Submit', 'Reset', 'Send']);
  });

  it('lets a role override the tag', () => {
    const outline = buildOutline(
      [
        { tag: 'a', role: 'button', text: 'Open menu', href: '#' },
        { tag: 'span', role: 'link', text: 'Fake link' },
        { tag: 'div', role: 'checkbox', ariaLabel: 'Agree' },
      ],
      BASE,
    );
    assert.deepEqual(outline, [
      { kind: 'button', text: 'Open menu' },
      { kind: 'link', text: 'Fake link' },
      { kind: 'field', text: 'Agree' },
    ]);
  });

  it('excludes hidden elements, hidden inputs and empty entries', () => {
    const outline = buildOutline(
      [
        { tag: 'h1', text: 'Hidden', hidden: true },
        { tag: 'input', type: 'hidden', name: 'csrf' },
        { tag: 'button', text: '   ' },
        { tag: 'h2', text: '' },
        null,
        'junk',
        { tag: 'h2', text: 'Shown' },
      ],
      BASE,
    );
    assert.deepEqual(outline, [{ kind: 'heading', text: 'Shown', level: 2 }]);
  });

  it('caps entries, text and href', () => {
    const raw = Array.from({ length: 100 }, (_, i) => ({ tag: 'a', text: 't'.repeat(500) + i, href: '/' + 'p'.repeat(5000) }));
    const outline = buildOutline(raw, BASE);
    assert.equal(outline.length, CAPS.outlineEntries);
    assert.ok(outline.every((e) => [...e.text].length === CAPS.outlineTextChars));
    assert.ok(outline.every((e) => [...e.href].length <= CAPS.urlChars));
  });

  it('is empty without input and is part of buildResult', () => {
    assert.deepEqual(buildOutline(undefined, BASE), []);
    const r = buildResult({ httpStatus: 200, finalUrl: BASE, outline: [{ tag: 'h1', text: 'Hi' }] });
    assert.deepEqual(r.outline, [{ kind: 'heading', text: 'Hi', level: 1 }]);
    assert.deepEqual(buildResult({ httpStatus: 200 }).outline, []);
  });
});

const SIGN_IN = Object.freeze({
  url: 'https://preview-abc.example.net/login',
  email: 'qa-admin@test.example',
  password: 'pa"ss\\<wörd>',
});

describe('sign_in input', () => {
  it('is null when absent', () => {
    assert.equal(normalizeInput(validInput()).signIn, null);
  });
  it('accepts a valid sign_in', () => {
    assert.deepEqual(normalizeInput(validInput({ sign_in: { ...SIGN_IN } })).signIn, SIGN_IN);
  });
  it('accepts the longest email and password', () => {
    const signIn = { ...SIGN_IN, email: 'e'.repeat(320), password: 'é'.repeat(200) };
    assert.deepEqual(normalizeInput(validInput({ sign_in: signIn })).signIn, signIn);
  });
  const bad = [
    ['url outside allowed_hosts', { url: 'https://other.example.net/login' }],
    ['http url', { url: 'http://preview-abc.example.net/login' }],
    ['url with a port', { url: 'https://preview-abc.example.net:8443/login' }],
    ['url with user info', { url: 'https://u:p@preview-abc.example.net/login' }],
    ['missing email', { email: undefined }],
    ['missing password', { password: undefined }],
    ['missing url', { url: undefined }],
    ['empty email', { email: '' }],
    ['empty password', { password: '' }],
    ['a line break in the email', { email: 'qa@test.example\n' }],
    ['a line break in the password', { password: 'pass\rword' }],
    ['a Unicode line break in the password', { password: 'pass\u2028word' }],
    ['a long email', { email: 'e'.repeat(321) }],
    ['a long password', { password: 'p'.repeat(201) }],
    ['a number password', { password: 1234 }],
    ['an unknown key', { otp: '123456' }],
  ];
  for (const [name, change] of bad) {
    it(`rejects ${name}, without quoting the password`, () => {
      const signIn = JSON.parse(JSON.stringify({ ...SIGN_IN, ...change }));
      assert.throws(
        () => normalizeInput(validInput({ sign_in: signIn })),
        (err) => /sign_in/.test(err.message) && !err.message.includes(SIGN_IN.password),
      );
    });
  }
  for (const v of [null, 'x', [], 1]) {
    it(`rejects sign_in ${JSON.stringify(v)}`, () => {
      assert.throws(() => normalizeInput(validInput({ sign_in: v })), /sign_in/);
    });
  }
  it('rejects sign_in with read_only true, accepts it with read_only false', () => {
    assert.throws(() => normalizeInput(validInput({ read_only: true, sign_in: { ...SIGN_IN } })), /read_only/);
    assert.ok(normalizeInput(validInput({ read_only: false, sign_in: { ...SIGN_IN } })).signIn);
  });
});

describe('sign-in fields', () => {
  const input = (o) => ({ type: '', autocomplete: '', name: '', id: '', visible: true, enabled: true, ...o });
  it('picks the first visible, enabled password and type=email fields', () => {
    const got = pickSignInFields([
      input({ type: 'search', name: 'q' }),
      input({ type: 'email', name: 'login', visible: false }),
      input({ type: 'email', name: 'address' }),
      input({ type: 'password', enabled: false }),
      input({ type: 'password', name: 'pw' }),
      input({ type: 'password', name: 'pw2' }),
    ]);
    assert.deepEqual(got, { password: 4, email: 2 });
  });
  it('prefers type=email over an earlier name match', () => {
    const got = pickSignInFields([input({ type: 'text', name: 'username' }), input({ type: 'email' }), input({ type: 'password' })]);
    assert.deepEqual(got, { password: 2, email: 1 });
  });
  it('falls back to a text-like field named for a user, email or login', () => {
    const cases = [
      [input({ autocomplete: 'username' }), 0],
      [input({ type: 'text', autocomplete: 'section-a email' }), 0],
      [input({ type: 'TEXT', name: 'UserName' }), 0],
      [input({ id: 'login-field' }), 0],
      [input({ type: 'text', name: 'Email_Address' }), 0],
      [input({ type: 'tel', name: 'user' }), -1],
      [input({ type: 'text', name: 'first-name' }), -1],
      [input({ type: 'text', name: 'user', visible: false }), -1],
      [input({ type: 'text', name: 'user', enabled: false }), -1],
    ];
    for (const [field, want] of cases) {
      assert.equal(pickSignInFields([field, input({ type: 'password' })]).email, want, JSON.stringify(field));
    }
  });
  it('never picks a hidden field', () => {
    const got = pickSignInFields([input({ type: 'email', visible: false }), input({ type: 'password', visible: false })]);
    assert.deepEqual(got, { password: -1, email: -1 });
  });
  it('plans both, email first, or fails with the reason', () => {
    assert.deepEqual(signInPlan({ password: 1, email: 0 }), { plan: 'both' });
    assert.deepEqual(signInPlan(pickSignInFields([input({ type: 'email' })])), { plan: 'email-first' });
    assert.deepEqual(signInPlan(pickSignInFields([input({ type: 'password' })])), {
      plan: 'fail',
      reason: 'the sign-in page has no email field',
    });
    assert.deepEqual(signInPlan(pickSignInFields([])), { plan: 'fail', reason: 'the sign-in page has no password field' });
  });
  it('sees the form as showing while a password field is visible, enabled or not', () => {
    assert.equal(hasVisiblePasswordField([input({ type: 'password', enabled: false })]), true);
    assert.equal(hasVisiblePasswordField([input({ type: 'password', visible: false }), input({ type: 'email' })]), false);
    assert.equal(hasVisiblePasswordField(null), false);
  });
  it('has the documented reasons', () => {
    assert.deepEqual({ ...SIGN_IN_REASONS }, {
      noPasswordField: 'the sign-in page has no password field',
      noEmailField: 'the sign-in page has no email field',
      stillOnForm: 'still on the sign-in form 10 seconds after submitting',
      loadFailed: 'the sign-in page could not be loaded: ',
    });
  });
});

describe('staysAfterSignIn', () => {
  const here = 'https://preview-abc.example.net/dashboard?tab=1';
  it('stays for the front page with no query on the host the browser is on', () => {
    assert.equal(staysAfterSignIn('https://preview-abc.example.net/', here), true);
    assert.equal(staysAfterSignIn('https://preview-abc.example.net', here), true);
    assert.equal(staysAfterSignIn('https://PREVIEW-abc.example.net/', here), true);
  });
  it('opens any other path, a query, or another host', () => {
    assert.equal(staysAfterSignIn('https://preview-abc.example.net/settings', here), false);
    assert.equal(staysAfterSignIn('https://preview-abc.example.net/?tab=2', here), false);
    assert.equal(staysAfterSignIn('https://preview-abc.example.net/', 'https://api-abc.example.net/'), false);
    assert.equal(staysAfterSignIn('https://preview-abc.example.net/', 'chrome-error://chromewebdata/'), false);
  });
});

describe('refused cookies', () => {
  const hosts = ['app.example.net', 'api.example.net'];
  const setCookie = (name, reason, value = 'secret-value') => ({
    blockedReasons: [reason, 'Other'],
    cookieLine: `${name}=${value}; Path=/; HttpOnly`,
    cookie: { name, value },
  });
  it('reports name, host and first reason, never a value', () => {
    const rc = new RefusedCookies(hosts);
    rc.request('1', 'https://api.example.net/auth/login');
    rc.response('1', true);
    rc.refused('1', 'response', { requestId: '1', blockedCookies: [setCookie('session', 'SameSiteLax')] });
    assert.deepEqual(rc.entries, [{ host: 'api.example.net', name: 'session', reason: 'SameSiteLax' }]);
    assert.ok(!JSON.stringify(rc.entries).includes('secret-value'));
  });
  it('takes the name from cookieLine when there is no parsed cookie', () => {
    const rc = new RefusedCookies(hosts);
    rc.request('1', 'https://api.example.net/');
    rc.response('1', true);
    rc.refused('1', 'response', {
      blockedCookies: [
        { blockedReasons: ['SyntaxError'], cookieLine: ' sid =abc=def; Secure' },
        { blockedReasons: ['SyntaxError'], cookieLine: 'novalue-is-the-value' },
      ],
    });
    assert.deepEqual(rc.entries, [
      { host: 'api.example.net', name: 'sid', reason: 'SyntaxError' },
      { host: 'api.example.net', name: '', reason: 'SyntaxError' },
    ]);
  });
  it('reports cookies refused for a request, not the ones sent', () => {
    const rc = new RefusedCookies(hosts);
    rc.request('2', 'https://api.example.net/me');
    rc.response('2', true);
    rc.refused('2', 'request', {
      associatedCookies: [
        { blockedReasons: [], cookie: { name: 'ok', value: 'v' } },
        { blockedReasons: ['ThirdPartyPhaseout'], cookie: { name: 'session', value: 'v' } },
      ],
    });
    assert.deepEqual(rc.entries, [{ host: 'api.example.net', name: 'session', reason: 'ThirdPartyPhaseout' }]);
  });
  it('matches an event that comes before its request', () => {
    const rc = new RefusedCookies(hosts);
    rc.refused('3', 'response', { blockedCookies: [setCookie('a', 'SameSiteStrict')] });
    assert.deepEqual(rc.entries, []);
    rc.request('3', 'https://app.example.net/');
    assert.deepEqual(rc.entries, []);
    rc.response('3', true);
    assert.deepEqual(rc.entries, [{ host: 'app.example.net', name: 'a', reason: 'SameSiteStrict' }]);
  });
  it('only reports hosts in allowed_hosts', () => {
    const rc = new RefusedCookies(hosts);
    rc.request('4', 'https://tracker.example.com/pixel');
    rc.response('4', true);
    rc.refused('4', 'response', { blockedCookies: [setCookie('uid', 'ThirdPartyPhaseout')] });
    assert.deepEqual(rc.entries, []);
  });
  it('attributes each ExtraInfo event to its redirect hop', () => {
    const rc = new RefusedCookies(hosts);
    rc.request('5', 'https://api.example.net/login');
    rc.request('5', 'https://app.example.net/home');
    // The first hop's events arrive after the second hop started.
    rc.refused('5', 'request', { associatedCookies: [{ blockedReasons: ['SameSiteLax'], cookie: { name: 'pre', value: 'v' } }] });
    rc.refused('5', 'response', { blockedCookies: [setCookie('api', 'SameSiteLax')] });
    rc.refused('5', 'response', { blockedCookies: [setCookie('app', 'SameSiteStrict')] });
    rc.response('5', true);
    assert.deepEqual(rc.entries, [
      { host: 'api.example.net', name: 'pre', reason: 'SameSiteLax' },
      { host: 'api.example.net', name: 'api', reason: 'SameSiteLax' },
      { host: 'app.example.net', name: 'app', reason: 'SameSiteStrict' },
    ]);
  });
  it('holds a hop’s event until that hop is seen', () => {
    const rc = new RefusedCookies(hosts);
    rc.request('6', 'https://tracker.example.com/r');
    rc.refused('6', 'response', { blockedCookies: [] });
    rc.refused('6', 'response', { blockedCookies: [setCookie('late', 'SameSiteLax')] });
    assert.deepEqual(rc.entries, []);
    rc.request('6', 'https://api.example.net/landing');
    rc.response('6', true);
    assert.deepEqual(rc.entries, [{ host: 'api.example.net', name: 'late', reason: 'SameSiteLax' }]);
  });
  it('skips a redirect hop without extra info', () => {
    const rc = new RefusedCookies(hosts);
    rc.request('8', 'https://app.example.net/cached-redirect');
    rc.request('8', 'https://api.example.net/login', false);
    rc.refused('8', 'request', { associatedCookies: [{ blockedReasons: ['SameSiteLax'], cookie: { name: 'req', value: 'v' } }] });
    rc.refused('8', 'response', { blockedCookies: [setCookie('session', 'SameSiteLax')] });
    rc.response('8', true);
    assert.deepEqual(rc.entries, [
      { host: 'api.example.net', name: 'req', reason: 'SameSiteLax' },
      { host: 'api.example.net', name: 'session', reason: 'SameSiteLax' },
    ]);
  });
  it('waits for the hop’s response before attributing past an unknown hop', () => {
    const rc = new RefusedCookies(hosts);
    rc.request('9', 'https://app.example.net/start');
    // The first hop's response is not known yet: the event could be its or a later hop's.
    rc.refused('9', 'response', { blockedCookies: [setCookie('late', 'SameSiteLax')] });
    assert.deepEqual(rc.entries, []);
    rc.request('9', 'https://api.example.net/next', false);
    assert.deepEqual(rc.entries, []);
    rc.response('9', true);
    assert.deepEqual(rc.entries, [{ host: 'api.example.net', name: 'late', reason: 'SameSiteLax' }]);
  });
  it('drops events of a hop whose response has no extra info', () => {
    const rc = new RefusedCookies(hosts);
    rc.request('10', 'https://api.example.net/');
    rc.response('10', false);
    rc.refused('10', 'response', { blockedCookies: [setCookie('x', 'SameSiteLax')] });
    rc.flush();
    assert.deepEqual(rc.entries, []);
  });
  it('flush attributes events of a hop whose response never came', () => {
    const rc = new RefusedCookies(hosts);
    rc.request('11', 'https://api.example.net/slow');
    rc.refused('11', 'request', { associatedCookies: [{ blockedReasons: ['SameSiteStrict'], cookie: { name: 's', value: 'v' } }] });
    assert.deepEqual(rc.entries, []);
    rc.flush();
    assert.deepEqual(rc.entries, [{ host: 'api.example.net', name: 's', reason: 'SameSiteStrict' }]);
  });
  it('keeps long names whole, so distinct ones stay distinct', () => {
    const rc = new RefusedCookies(hosts);
    rc.request('7', 'https://api.example.net/');
    rc.response('7', true);
    const a = 'n'.repeat(301) + 'a';
    const b = 'n'.repeat(301) + 'b';
    rc.refused('7', 'response', { blockedCookies: [setCookie(a, 'SameSiteLax'), setCookie(b, 'SameSiteLax')] });
    assert.deepEqual(rc.entries.map((e) => e.name), [a, b]);
  });
  it('deduplicates and caps at 20', () => {
    const rc = new RefusedCookies(hosts);
    for (let i = 0; i < 30; i++) {
      rc.request(String(i), 'https://api.example.net/x');
      rc.response(String(i), true);
      rc.refused(String(i), 'response', { blockedCookies: [setCookie('dup', 'SameSiteLax'), setCookie(`c${i}`, 'SameSiteLax')] });
    }
    assert.equal(CAPS.refusedCookies, 20);
    assert.equal(rc.entries.length, 20);
    assert.equal(rc.entries.filter((e) => e.name === 'dup').length, 1);
  });
});

describe('sign-in boundary', () => {
  const failure = (path) =>
    firstPartyFailure({ url: `https://preview-abc.example.net${path}`, method: 'GET', status: 401 }, HOSTS);
  const collector = () => ({
    failedRequests: [],
    signIn: { signedIn: true, signedOutOnLoad: false, failedRequests: [], refusedCookies: [] },
  });
  it('files a request that started during the sign-in as the sign-in’s', () => {
    const c = collector();
    recordFailedRequest(c, failure('/api/me'), true);
    assert.deepEqual(c.failedRequests, []);
    assert.equal(c.signIn.failedRequests.length, 1);
    const r = buildResult({ ...c, httpStatus: 200, steps: [], assertions: [] });
    assert.equal(r.failed_requests.length, 0);
    assert.equal(r.sign_in.failed_requests[0].status, 401);
    assert.equal(r.verdict, 'pass');
  });
  it('files a request that started after it as the check’s', () => {
    const c = collector();
    recordFailedRequest(c, failure('/api/orders'), false);
    recordFailedRequest(c, null, false);
    assert.equal(c.failedRequests.length, 1);
    assert.equal(buildResult({ ...c, httpStatus: 200 }).verdict, 'fail');
  });
  it('caps the sign-in’s failed requests', () => {
    const c = collector();
    for (let i = 0; i < 30; i++) {
      recordFailedRequest(c, failure(`/${i}`), true);
    }
    assert.equal(c.signIn.failedRequests.length, CAPS.failedRequests);
  });
});

describe('sign_in result', () => {
  const signIn = (o) => ({ signedIn: true, reason: '', signedOutOnLoad: false, failedRequests: [], refusedCookies: [], ...o });
  it('has no sign_in without one', () => {
    const r = buildResult({ httpStatus: 200, steps: [{ action: 'click', ok: true }], assertions: [] });
    assert.equal('sign_in' in r, false);
    assert.equal(r.verdict, 'pass');
  });
  it('reports signed in with signed_out_on_load and no reason', () => {
    const cookie = { host: 'preview-abc.example.net', name: 'sid', reason: 'SameSiteLax' };
    const r = buildResult({ httpStatus: 200, signIn: signIn({ signedOutOnLoad: true, refusedCookies: [cookie] }) });
    assert.deepEqual(r.sign_in, { signed_in: true, signed_out_on_load: true, failed_requests: [], refused_cookies: [cookie] });
    assert.equal(r.verdict, 'pass', 'refused cookies and signed_out_on_load decide nothing on their own');
  });
  it('reports a failed sign-in with its reason, every step and assertion not run, and fails', () => {
    const r = buildResult({
      httpStatus: 0,
      steps: [{ action: 'click', ok: false }, { action: 'wait_for', ok: false }],
      assertions: [{ type: 'text_visible', passed: false }],
      signIn: signIn({ signedIn: false, reason: SIGN_IN_REASONS.stillOnForm }),
    });
    assert.deepEqual(r.sign_in, {
      signed_in: false,
      reason: 'still on the sign-in form 10 seconds after submitting',
      failed_requests: [],
      refused_cookies: [],
    });
    assert.deepEqual(
      r.steps.map((s) => [s.index, s.action, s.ok, s.error]),
      [
        [0, 'click', false, SIGN_IN_NOT_RUN],
        [1, 'wait_for', false, SIGN_IN_NOT_RUN],
      ],
    );
    assert.equal(SIGN_IN_NOT_RUN, 'not run: the sign-in failed');
    assert.deepEqual(r.assertions, [{ index: 0, type: 'text_visible', passed: false, detail: SIGN_IN_NOT_RUN }]);
    assert.equal(r.verdict, 'fail');
  });
  it('fails a failed sign-in with no steps or assertions', () => {
    const r = buildResult({ httpStatus: 200, signIn: signIn({ signedIn: false, reason: SIGN_IN_REASONS.noEmailField }) });
    assert.equal(r.verdict, 'fail');
  });
  it('caps the sign-in’s lists', () => {
    const many = (n, f) => Array.from({ length: n }, (_, i) => f(i));
    const r = buildResult({
      signIn: signIn({
        failedRequests: many(25, (i) => ({ url: `u${i}`, method: 'GET', status: 401, failure: '' })),
        refusedCookies: many(25, (i) => ({ host: 'h', name: `c${i}`, reason: 'SameSiteLax' })),
      }),
    });
    assert.equal(r.sign_in.failed_requests.length, CAPS.failedRequests);
    assert.equal(r.sign_in.refused_cookies.length, CAPS.refusedCookies);
  });
  it('errorResult has no sign_in', () => {
    assert.equal('sign_in' in errorResult('boom'), false);
  });
});

describe('redactSecret', () => {
  afterEach(() => setSecret(''));
  it('redacts the secret as is and URL-encoded, in values and keys, at any depth', () => {
    const pw = 'p&ss wörd"<';
    setSecret(pw);
    const r = redactSecret({
      final_url: `https://x/login?email=a%40b&password=${new URLSearchParams({ p: pw }).toString().slice(2)}`,
      nested: [{ [`k${pw}`]: `v${pw}`, n: 3, b: true, z: null }],
      other: `https://x/?p=${encodeURIComponent(pw)}`,
      email: 'a@b',
    });
    assert.deepEqual(r, {
      final_url: 'https://x/login?email=a%40b&password=<redacted>',
      nested: [{ 'k<redacted>': 'v<redacted>', n: 3, b: true, z: null }],
      other: 'https://x/?p=<redacted>',
      email: 'a@b',
    });
  });
  it('leaves the result alone without a secret', () => {
    const v = { a: 'x' };
    assert.equal(redactSecret(v), v);
  });
  it('redacts before a cap or whitespace collapse could cut or reshape the password', () => {
    const pw = 'hunter2  pass';
    setSecret(pw);
    const c = { httpStatus: 200 };
    c.domText = 'x'.repeat(CAPS.domTextChars - 5) + pw;
    c.pageErrors = ['e'.repeat(CAPS.pageErrorChars - 5) + pw];
    c.consoleErrors = ['c'.repeat(CAPS.consoleErrorChars - 5) + pw];
    c.finalUrl = 'https://x/?q=' + 'u'.repeat(CAPS.urlChars - 20) + encodeURIComponent(pw);
    const r = buildResult(c);
    for (const v of [r.dom_text_snippet, r.page_errors[0], r.console_errors[0]]) {
      assert.ok(!v.includes('hunt'), v.slice(-40));
    }
    assert.ok(!r.final_url.includes('hunt'));
    assert.equal(oneLine(`failed: ${pw}`), 'failed: <redacted>');
    const failed = firstPartyFailure({ url: `https://preview-abc.example.net/${'p'.repeat(CAPS.urlChars - 40)}?pw=${encodeURIComponent(pw)}`, status: 500 }, HOSTS);
    assert.ok(!failed.url.includes('hunt'));
  });
});

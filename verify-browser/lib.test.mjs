import assert from 'node:assert/strict';
import { describe, it } from 'node:test';
import {
  CAPS,
  LIMITS,
  abortedByNavigation,
  blameBlockedNavigation,
  buildOutline,
  buildResult,
  capString,
  computeVerdict,
  errorResult,
  firstPartyFailure,
  isAllowedUrl,
  normalizeInput,
  pushCapped,
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
  const bad = [
    ['schema_version', { schema_version: 2 }],
    ['non-https url', { url: 'http://preview-abc.example.net/' }],
    ['url outside allowed_hosts', { url: 'https://other.example.net/' }],
    ['goto outside allowed_hosts', { steps: [{ action: 'goto', url: 'https://www.iana.org/' }] }],
    ['unknown action', { steps: [{ action: 'hover', selector: 'a' }] }],
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
    assert.deepEqual(r, { schema_version: 1, status: 'error', error: 'boom at x' });
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

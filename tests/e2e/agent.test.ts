import {env} from 'node:process';
import {mkdir, rm, stat} from 'node:fs/promises';
import {createServer} from 'node:http';
import type {AddressInfo} from 'node:net';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {test, expect} from '@playwright/test';
import type {APIRequestContext, Page} from '@playwright/test';
import {apiAddCollaborator, apiCreateFiles, apiCreateRepo, apiHeaders, apiSetPages, baseUrl, login, randomString} from './utils.ts';

const user = env.GITEA_TEST_E2E_USER;
const unreachableModel = 'http://127.0.0.1:9/v1';
// the account the e2e instance names as its agent (see tools/test-e2e.sh): every tool of a run
// works as this account, so a repository the spec drives has to let it in
const agent = 'e2e-agent';
const agentPassword = 'e2e-agent-password';
let agentReady = false;
const modelId = 'e2e-model';
const message = 'what does this repository hold';

/** Point the repository role at a provider that cannot answer: a run of this instance always
 * fails at the model call, which is the state the chat surface is asserted on. */
/** The agent account and its seat on a repository: without both, a run of this spec has no
 * rights to work with and fails before it can do anything. */
async function ensureAgent(request: APIRequestContext, repo: string) {
  if (!agentReady) {
    const created = await request.post(`${baseUrl()}/api/v1/admin/users`, {
      headers: apiHeaders(),
      data: {username: agent, password: agentPassword, email: `${agent}@${env.GITEA_TEST_E2E_DOMAIN}`, must_change_password: false},
    });
    // both browser projects share one instance, so either may have created it already
    const complaint = created.ok() ? '' : await created.text();
    expect(created.ok() || complaint.includes('already exists'), complaint).toBe(true);
    agentReady = true;
  }
  await apiAddCollaborator(request, user, repo, agent, {headers: apiHeaders()});
}

/** A stored credential of its own, so a provider the spec points somewhere is really called. */
async function storeKey(request: APIRequestContext): Promise<string> {
  const name = `e2e-key-${randomString(6)}`;
  const stored = await request.post('/-/admin/agent/keys/new', {form: {name, value: 'e2e-value'}, maxRedirects: 0});
  expect(stored.status(), await stored.text()).toBe(303);
  const page = await (await request.get(`/-/admin/agent/keys?q=${name}`)).text();
  const keyId = /\/-\/admin\/agent\/keys\/(\d+)/.exec(page)?.[1];
  expect(keyId, 'the stored key is not on the keys page').toBeTruthy();
  return keyId!;
}

async function setRepositoryModel(request: APIRequestContext, baseUrl = unreachableModel) {
  const name = `e2e-${randomString(6)}`;
  const created = await request.post('/-/admin/agent/providers/new', {
    form: {
      name,
      provider: 'openai',
      base_url: baseUrl,
      api_key_id: await storeKey(request),
      manual_models: modelId,
      headers: '',
    },
    maxRedirects: 0,
  });
  expect(created.status(), await created.text()).toBe(303);
  // the list is paged, so the new provider is found by its own name rather than by position
  const providers = await (await request.get(`/-/admin/agent/providers?q=${name}`)).text();
  const providerId = /\/-\/admin\/agent\/providers\/(\d+)\/edit/.exec(providers)?.[1];
  expect(providerId, 'the new provider is not on the providers page').toBeTruthy();

  const ref = `provider:${providerId}/${modelId}`;
  await request.post('/-/admin/agent/roles', {form: {role_repository: ref}, maxRedirects: 0});
  const roles = await (await request.get('/-/admin/agent/roles')).text();
  expect(roles, 'the role was not kept').toContain(`value="${ref}" selected`);
}

async function repoIdOf(request: APIRequestContext, name: string): Promise<number> {
  const response = await request.get(`${baseUrl()}/api/v1/repos/${user}/${name}`, {headers: apiHeaders()});
  expect(response.ok(), `could not read ${name}: ${response.status()}`).toBe(true);
  return (await response.json()).id as number;
}

/** A session opened the way the new-conversation control opens one, without a first message. */
async function openSession(request: APIRequestContext, repoId: number): Promise<number> {
  const response = await request.post('/-/agent/new', {form: {kind: 'repository', repo_id: String(repoId)}, maxRedirects: 0});
  const session = (response.headers()['location'] ?? '').split('/').pop();
  expect(Number(session), `the new session answered ${response.status()} ${response.headers()['location']}`).toBeGreaterThan(0);
  return Number(session);
}

/** The permissions the tools page shows, as the form would post them back. */
async function readPermissions(page: Page) {
  return await page.evaluate(() => {
    const modes: Record<string, string> = {};
    const enabled: Record<string, boolean> = {};
    for (const select of document.querySelectorAll<HTMLSelectElement>('select[name^="mode_"]')) {
      modes[select.name.slice('mode_'.length)] = select.value;
    }
    for (const box of document.querySelectorAll<HTMLInputElement>('input[name^="enabled_"]')) {
      enabled[box.name.slice('enabled_'.length)] = box.checked;
    }
    return {modes, enabled};
  });
}

type StubReply = {content?: string; reasoning?: string; calls?: Array<{name: string; args: string}>};
type ModelStub = {url: string; asked: Array<any>; close: () => Promise<void>; thought: () => void; piece: (text: string) => void; answer: () => void};

/** A model gateway this spec scripts. The instance talks to whatever endpoint its provider
 * points at, so a stub is how a run can be made to call a tool or to answer on demand. */
async function startModelStub(answer: (body: any, index: number) => StubReply, {hold = false} = {}): Promise<ModelStub> {
  const asked: Array<any> = [];
  // a held stream the test lets out one frame at a time, so it can look at the page between them
  let sayThought: (() => void) | undefined;
  let sayPiece: ((text: string) => void) | undefined;
  let sayAnswer: (() => void) | undefined;
  const server = createServer((req, res) => {
    const chunks: Array<Buffer> = [];
    req.on('data', (chunk: Buffer) => {
      chunks.push(chunk);
    });
    req.on('end', () => {
      const body = JSON.parse(Buffer.concat(chunks).toString() || '{}');
      asked.push(body);
      const reply = answer(body, asked.length - 1);
      // the wire shape names the tool's arguments, not the script's shorthand
      const calls = (reply.calls ?? []).map((call, index) => ({index, id: `call_${index}`, type: 'function', function: {name: call.name, arguments: call.args}}));
      const message = calls.length ?
        {role: 'assistant', tool_calls: calls, finish_reason: 'tool_calls'} :
        {role: 'assistant', content: reply.content ?? '', finish_reason: 'stop'};
      if (body.stream) {
        const frame = (delta: Record<string, unknown>) => `data: ${JSON.stringify({model: body.model, choices: [{index: 0, delta, finish_reason: null}]})}\n\n`;
        const thought = calls.length ? {} : {reasoning: reply.reasoning};
        const said = calls.length ? {tool_calls: calls} : {content: message.content};
        res.writeHead(200, {'Content-Type': 'text/event-stream'});
        if (hold) {
          // the stream is held open: the test says when the thought goes out, when each piece of
          // the answer does, and when the answer ends, so what the page does with each is seen
          let sentThought = false;
          let sentPiece = false;
          sayThought = () => {
            sentThought = true;
            res.write(frame({reasoning: reply.reasoning}));
          };
          sayPiece = (text: string) => {
            sentPiece = true;
            res.write(frame({content: text}));
          };
          sayAnswer = () => {
            const opening = sentThought ? {} : thought;
            const closing = sentPiece ? {content: ''} : said;
            res.end(`${frame({...opening, ...closing})}data: [DONE]\n\n`);
          };
          return;
        }
        res.end(`${frame({...thought, ...said})}data: [DONE]\n\n`);
        return;
      }
      res.writeHead(200, {'Content-Type': 'application/json'});
      res.end(JSON.stringify({id: 'stub', model: body.model, choices: [{index: 0, message, finish_reason: message.finish_reason}]}));
    });
  });
  const listening = Promise.withResolvers<void>();
  server.listen(0, '127.0.0.1', () => listening.resolve());
  await listening.promise;
  const closed = () => {
    const done = Promise.withResolvers<void>();
    server.close(() => done.resolve());
    return done.promise;
  };
  const url = `http://127.0.0.1:${(server.address() as AddressInfo).port}/v1`;
  return {url, asked, close: closed, thought: () => sayThought?.(), piece: (text) => sayPiece?.(text), answer: () => sayAnswer?.()};
}

/** The whole permissions form, so one tool can be changed without emptying the others. */
async function postPermissions(page: Page, change: (form: Record<string, string>) => void) {
  await page.goto('/-/admin/agent/tools');
  const before = await readPermissions(page);
  const form: Record<string, string> = {};
  for (const [name, mode] of Object.entries(before.modes)) form[`mode_${name}`] = mode;
  for (const [name, on] of Object.entries(before.enabled)) {
    if (on) form[`enabled_${name}`] = 'true';
  }
  change(form);
  const posted = await page.request.post('/-/admin/agent/tools', {form, maxRedirects: 0});
  expect([302, 303], await posted.text()).toContain(posted.status());
}

async function enableTool(page: Page, name: string, mode: string) {
  await postPermissions(page, (form) => {
    form[`mode_${name}`] = mode;
    form[`enabled_${name}`] = 'true';
  });
}

/** A second account, so a guard can be asked whether it keeps one reader out of another's work. */
async function createUser(request: APIRequestContext, username: string, password: string) {
  const created = await request.post(`${baseUrl()}/api/v1/admin/users`, {
    headers: apiHeaders(),
    data: {username, password, email: `${username}@${env.GITEA_TEST_E2E_DOMAIN}`, must_change_password: false},
  });
  expect(created.ok(), await created.text()).toBe(true);
}

/** A session of the given repository, opened the way the new-conversation control opens one. */
async function sessionOf(request: APIRequestContext, repoId: number): Promise<number> {
  const response = await request.post('/-/agent/new', {form: {kind: 'repository', repo_id: String(repoId)}, maxRedirects: 0});
  return Number((response.headers()['location'] ?? '').split('/').pop());
}

function entries<T>(record: Record<string, T>, names: Array<string>): Record<string, T> {
  return Object.fromEntries(names.map((name) => [name, record[name]]));
}

/** The tools, the roles and the providers are instance-wide state, and every browser project
 * runs against the same instance: without a lock the other project's write reads as ours. */
async function withInstanceLock<T>(fn: () => Promise<T>): Promise<T> {
  const lock = join(tmpdir(), 'gitea-e2e-agent-tools.lock');
  const deadline = Date.now() + 90_000;
  for (;;) {
    try {
      await mkdir(lock);
      break;
    } catch {
      let heldMs: number | undefined;
      try {
        heldMs = (await stat(lock)).mtimeMs; // the holder may have left between the mkdir and the stat
      } catch {
        heldMs = undefined;
      }
      if (heldMs && Date.now() - heldMs > 60_000) {
        await rm(lock, {recursive: true, force: true}); // a run that died holding it
      } else if (Date.now() > deadline) {
        throw new Error('the tools lock was never released');
      }
      // two browser processes share no clock: a contended lock can only be waited out
      const {promise, resolve} = Promise.withResolvers<void>();
      setTimeout(resolve, 100);
      await promise;
    }
  }
  try {
    return await fn();
  } finally {
    await rm(lock, {recursive: true, force: true});
  }
}

test.describe.serial('agent', () => {
  let repoName = '';
  let sessionId = 0;

  test('a repository session takes a message the model then fails on', async ({page, request}) => {
    await login(page);
    repoName = `e2e-agent-${randomString(8)}`;
    await apiCreateRepo(request, {name: repoName});
    await ensureAgent(request, repoName);

    await withInstanceLock(async () => {
      await setRepositoryModel(page.request);
      sessionId = await openSession(page.request, await repoIdOf(request, repoName));

      await page.goto(`/-/agent/${sessionId}`);
      await page.locator('.agent-chat-input').click();
      await page.keyboard.type(message);
      await page.locator('.agent-chat-send').click();

      // the reader's own message arrives over the live stream, and the answer under it fails
      await expect(page.locator('.agent-chat-message.user').filter({hasText: message})).toBeVisible();
      await page.reload();
      await expect(page.locator('.agent-chat-pending-status')).toBeVisible();
      await expect(page.locator('.agent-chat-pending-label')).not.toBeEmpty();
    });

    await page.goto('/-/agent');
    await expect(page.locator('a.agent-chat-card-new')).toBeVisible();

    // the session is listed where it belongs: under the repository it works in
    await page.goto('/-/agent?scope=repository');
    const group = page.locator('.agent-repo-group', {hasText: repoName});
    await group.locator('.agent-repo-group-head').click();
    await expect(group.locator(`a.agent-chat-card-main[href$="/-/agent/${sessionId}"]`)).toBeVisible();
  });

  test('the tools page keeps every permission it does not change', async ({page}) => {
    await login(page);
    await withInstanceLock(async () => {
      await page.goto('/-/admin/agent/tools');

      const before = await readPermissions(page);
      const names = Object.keys(before.modes);
      expect(names.length).toBeGreaterThan(0);
      const flipped = names[0];

      const form: Record<string, string> = {};
      for (const name of names) {
        form[`mode_${name}`] = before.modes[name];
        // a checkbox that is not posted is a tool switched off, which is how a partial form once
        // emptied the page: every tool has to be sent, with the state it had
        if (name === flipped ? !before.enabled[name] : before.enabled[name]) form[`enabled_${name}`] = 'true';
      }
      const posted = await page.request.post('/-/admin/agent/tools', {form, maxRedirects: 0});
      expect([302, 303], await posted.text()).toContain(posted.status());

      await page.goto('/-/admin/agent/tools');
      const after = await readPermissions(page);
      expect(after.enabled[flipped]).toBe(!before.enabled[flipped]);
      expect(after.modes[flipped]).toBe(before.modes[flipped]);
      const others = names.filter((name) => name !== flipped);
      expect(entries(after.enabled, others)).toEqual(entries(before.enabled, others));
      expect(entries(after.modes, others)).toEqual(entries(before.modes, others));
    });
  });

  test('the runs page lists the run of that session', async ({page}) => {
    await login(page);
    await page.goto('/-/admin/agent/skills');
    await expect(page.locator('button.show-modal[data-modal="#agent-skill-modal"]')).toBeVisible();

    // the roles page offers the tiers an instance pays for, and the ways to split the roles
    await page.goto('/-/admin/agent/roles');
    // the selects are the native fields Fomantic replaces, so they are present rather than visible
    await expect(page.locator('select[name="cheap"]')).toHaveCount(1);
    await expect(page.locator('select[name="strong"]')).toHaveCount(1);
    await expect(page.locator('select[name="decision"]')).toHaveCount(1);
    await expect(page.locator('button[name="preset"][value="balanced"]')).toBeVisible();
    await expect(page.locator('select[name="role_decision"]')).toHaveCount(1);

    await page.goto('/-/admin/agent/runs');
    const row = page.locator('.agent-runs tbody tr[data-run-id]').first();
    await expect(row).toBeVisible();
    // every row can be asked what its run was handed, whether or not that one was traced
    await expect(row.locator('a[href$="/trace"]')).toBeVisible();
  });

  test('pages are published from a branch, and the repository links them', async ({page, request}) => {
    await login(page);
    const name = `e2e-pages-${randomString(8)}`;
    await apiCreateRepo(request, {name});
    const body = '<!doctype html><html lang="en"><head><title>e2e</title></head><body><h1>the published page</h1></body></html>';
    await apiCreateFiles(request, user, name, [{path: 'public/index.html', content: body}], {branch: 'main'});

    // nothing is published until the repository says how it publishes
    expect((await page.request.get(`/${user}/${name}/-/pages/`)).status()).toBe(404);
    await apiSetPages(request, user, name, {source: 'branch', branch: 'main', dir: '/public'});

    const served = await page.goto(`/${user}/${name}/-/pages/`);
    expect(served?.headers()['content-type']).toContain('text/html');
    expect(await served?.text()).toContain('the published page');

    // the link to the pages sits with the description of the repository, not in the tab bar
    await page.goto(`/${user}/${name}`);
    await expect(page.locator(`a[href$="/${name}/-/pages/"]`)).toBeVisible();
    await expect(page.locator(`header a[href$="/${name}/-/pages/"]`)).toHaveCount(0);

    // the settings page says where the site is served and how it is published
    await page.goto(`/${user}/${name}/settings/pages`);
    await expect(page.locator(`a[href$="/${name}/-/pages/"]`).first()).toBeVisible();
    await expect(page.locator('input[name="source"][value="branch"]')).toBeChecked();
  });
});

test.describe.serial('agent surfaces and guards', () => {
  let repoName = '';

  test('a scripted model answers, and the request carries the tools', async ({page, request}) => {
    await login(page);
    repoName = `e2e-answer-${randomString(8)}`;
    await apiCreateRepo(request, {name: repoName});
    await ensureAgent(request, repoName);
    const repo = await repoIdOf(request, repoName);

    const stub = await startModelStub(() => ({content: 'the stub answered'}));
    try {
      await withInstanceLock(async () => {
        await setRepositoryModel(page.request, stub.url);
        const session = await sessionOf(page.request, repo);
        await page.goto(`/-/agent/${session}`);
        await page.locator('.agent-chat-input').click();
        await page.keyboard.type('say something');
        await page.locator('.agent-chat-send').click();
        await expect(page.locator('.agent-chat-message.assistant').filter({hasText: 'the stub answered'})).toBeVisible();
      });
    } finally {
      await stub.close();
    }
    expect(stub.asked).toHaveLength(1);
    expect(stub.asked[0].tools.length).toBeGreaterThan(1);
    expect(stub.asked[0].messages[0].role).toBe('system');
  });

  test('a manual write waits for its reader, and the card allows it', async ({page, request}) => {
    await login(page);
    const name = `e2e-allow-${randomString(8)}`;
    await apiCreateRepo(request, {name});
    await ensureAgent(request, name);
    const repo = await repoIdOf(request, name);

    const stub = await startModelStub((body) => {
      if (body.messages.some((message: any) => message.role === 'tool')) return {content: 'the change is in'};
      return {calls: [{name: 'gitea_commit_files', args: JSON.stringify({
        branch: 'e2e-allowed', message: 'the approved change', files: [{path: 'docs/allowed.md', content: 'allowed\n'}],
      })}]};
    });
    try {
      await withInstanceLock(async () => {
        await enableTool(page, 'gitea_commit_files', 'manual');
        await setRepositoryModel(page.request, stub.url);
        const session = await sessionOf(page.request, repo);
        await page.goto(`/-/agent/${session}`);
        await page.request.post(`/-/agent/${session}/messages`, {form: {content: 'commit a note'}, maxRedirects: 0});
        await page.reload();
        const card = page.locator('.agent-chat-approval');
        await expect(card).toBeVisible();
        await expect(card.locator('.agent-chat-approval-name')).toHaveText('gitea_commit_files');
        await card.getByRole('button', {name: 'Allow'}).click();
        await expect(page.locator('.agent-chat-message.assistant').filter({hasText: 'the change is in'})).toBeVisible();
      });
    } finally {
      await stub.close();
    }
    const written = await request.get(`${baseUrl()}/api/v1/repos/${user}/${name}/contents/docs/allowed.md?ref=e2e-allowed`, {headers: apiHeaders()});
    expect(written.ok(), await written.text()).toBe(true);
  });

  test('the approval page refuses a write, and nothing lands', async ({page, request}) => {
    await login(page);
    const name = `e2e-refuse-${randomString(8)}`;
    await apiCreateRepo(request, {name});
    await ensureAgent(request, name);
    const repo = await repoIdOf(request, name);

    const stub = await startModelStub((body) => {
      if (body.messages.some((message: any) => message.role === 'tool')) return {content: 'the write was refused'};
      return {calls: [{name: 'gitea_commit_files', args: JSON.stringify({
        branch: 'e2e-refused', message: 'the refused change', files: [{path: 'docs/refused.md', content: 'refused\n'}],
      })}]};
    });
    try {
      await withInstanceLock(async () => {
        await enableTool(page, 'gitea_commit_files', 'manual');
        await setRepositoryModel(page.request, stub.url);
        const session = await sessionOf(page.request, repo);
        await page.goto(`/-/agent/${session}`);
        await page.locator('.agent-chat-input').click();
        await page.keyboard.type('commit a note');
        await page.locator('.agent-chat-send').click();

        const card = page.locator('.agent-chat-approval');
        await expect(card).toBeVisible();
        const approvalId = await card.getAttribute('data-approval-id');

        // the page a thread links to shows the same call and decides it
        await page.goto(`/-/agent/approvals/${approvalId}`);
        await expect(page.locator('.agent-chat-approval-name')).toHaveText('gitea_commit_files');
        await page.getByRole('button', {name: 'Refuse'}).click();
        // the decision sends the reader back to the thread the call belongs to
        await expect(page).toHaveURL(new RegExp(`/-/agent/${session}$`));
        await page.goto(`/-/agent/${session}`);
        await expect(page.locator('.agent-chat-message.assistant').filter({hasText: 'the write was refused'})).toBeVisible();
        await expect(page.locator('.agent-chat-approval')).toHaveCount(0);
      });
    } finally {
      await stub.close();
    }
    const written = await request.get(`${baseUrl()}/api/v1/repos/${user}/${name}/contents/docs/refused.md?ref=e2e-refused`, {headers: apiHeaders()});
    expect(written.status()).toBe(404);
  });

  test('another account reaches neither a session nor a draft it may not have', async ({page, browser, request}) => {
    await login(page);
    const name = `e2e-guard-${randomString(8)}`;
    await apiCreateRepo(request, {name});
    const repo = await repoIdOf(request, name);
    const session = await sessionOf(page.request, repo);

    const stranger = `e2e-stranger-${randomString(6)}`;
    const password = 'stranger-password';
    await createUser(request, stranger, password);

    // a reader of their own, so nothing is read with the admin's session
    await using theirs = await browser.newContext();
    {
      const reader = await theirs.newPage();
      await login(reader, stranger, password);
      const basic = `Basic ${Buffer.from(`${stranger}:${password}`).toString('base64')}`;
      const who = await (await reader.request.get(`${baseUrl()}/api/v1/user`, {headers: {Authorization: basic}})).json();
      expect(who).toMatchObject({login: stranger, is_admin: false});

      // a session of someone else is indistinguishable from one that does not exist
      expect((await reader.request.get(`/-/agent/${session}`)).status()).toBe(404);
      // the draft page opens, but only an admin may start a session that is not a repository's
      expect((await reader.request.get('/-/agent/new')).status()).toBe(200);
      expect((await reader.request.post('/-/agent/new', {form: {kind: 'server'}, maxRedirects: 0})).status()).toBe(404);
      expect((await reader.request.post('/-/agent/new', {form: {kind: 'repository', repo_id: String(repo)}, maxRedirects: 0})).status()).toBe(404);
      // the admin still reaches what the stranger may not
      expect((await page.request.get(`/-/agent/${session}`)).status()).toBe(200);
    }
  });

  test('pages are served from the configured directory only', async ({page, request}) => {
    await login(page);
    const name = `e2e-pages-root-${randomString(8)}`;
    await apiCreateRepo(request, {name});
    await apiCreateFiles(request, user, name, [
      {path: 'public/index.html', content: '<html><body><h1>index</h1></body></html>'},
      {path: 'public/docs/index.html', content: '<html><body><h1>docs index</h1></body></html>'},
      {path: 'secret.txt', content: 'not for readers of the page'},
    ], {branch: 'main'});
    await apiSetPages(request, user, name, {source: 'branch', branch: 'main', dir: '/public'});

    const root = await page.request.get(`/${user}/${name}/-/pages/`);
    expect(root.status()).toBe(200);
    expect(root.headers()['content-type']).toContain('text/html');
    // a directory is served by its index, the way a web server does it
    const directory = await page.request.get(`/${user}/${name}/-/pages/docs/`);
    expect(directory.status()).toBe(200);
    expect(await directory.text()).toContain('docs index');
    // what is not under the configured directory is not served, and a missing file is not a page
    for (const path of ['secret.txt', '../secret.txt', 'missing.html']) {
      const response = await page.request.get(`/${user}/${name}/-/pages/${path}`);
      expect(response.status(), `${path} was served`).toBe(404);
    }
    const etag = root.headers()['etag'];
    expect(etag).toBeTruthy();
    const cached = await page.request.get(`/${user}/${name}/-/pages/`, {headers: {'If-None-Match': etag}});
    expect(cached.status()).toBe(304);

    // the same repository published from the root serves the file that was outside public/
    await apiSetPages(request, user, name, {source: 'branch', branch: 'main', dir: '/'});
    expect((await page.request.get(`/${user}/${name}/-/pages/secret.txt`)).status()).toBe(200);
  });

  test('a repository can publish its pages from a workflow instead', async ({page, request}) => {
    await login(page);
    const name = `e2e-pages-actions-${randomString(8)}`;
    await apiCreateRepo(request, {name});
    await apiCreateFiles(request, user, name, [{path: 'public/index.html', content: '<html><body><h1>from a branch</h1></body></html>'}], {branch: 'main'});
    await apiSetPages(request, user, name, {source: 'actions'});

    // from a workflow, nothing is served until a deployment was made
    expect((await page.request.get(`/${user}/${name}/-/pages/`)).status()).toBe(404);

    // the settings page offers the source a workflow uploads, and the deployments it made
    await page.goto(`/${user}/${name}/settings/pages`);
    await expect(page.locator('input[name="source"][value="actions"]')).toBeChecked();
    await expect(page.getByText('upload-artifact', {exact: false})).toBeVisible();

    // the API deploys an artifact of a workflow run, and refuses one that holds no page
    const refused = await request.post(`${baseUrl()}/api/v1/repos/${user}/${name}/pages/deploy`, {
      headers: apiHeaders(),
      data: {artifact_id: 999999},
    });
    expect(refused.status()).toBe(404);
  });

  test('a thought streamed to a reloaded page lands without an error', async ({page, request}) => {
    await login(page);
    const name = `e2e-thought-${randomString(8)}`;
    await apiCreateRepo(request, {name});
    await ensureAgent(request, name);
    const repo = await repoIdOf(request, name);

    // the gateway thinks out loud, and is held there until the test has looked at the page
    const stub = await startModelStub(() => ({reasoning: 'let me think about it', content: 'the stub answered'}), {hold: true});
    const errors: Array<string> = [];
    page.on('pageerror', (error) => {
      errors.push(error.message);
    });
    try {
      await withInstanceLock(async () => {
        await setRepositoryModel(page.request, stub.url);
        const session = await sessionOf(page.request, repo);
        await page.goto(`/-/agent/${session}`);
        await page.locator('.agent-chat-input').click();
        await page.keyboard.type('say something');
        await page.locator('.agent-chat-send').click();

        // the run is streaming and held, so the page reloads onto the block the server renders
        await expect.poll(() => stub.asked.length, {message: 'the run never reached the gateway'}).toBeGreaterThan(0);
        await page.reload();
        await expect(page.locator('.agent-chat-pending')).toBeVisible();
        await expect(page.locator('.agent-chat-thought')).toHaveCount(0); // the server knows no thought yet

        // the thought now lands in that block, and nothing writes into a missing element
        stub.thought();
        await expect(page.locator('.agent-chat-thought pre')).toContainText('let me think about it');
        expect(errors, errors.join(' | ')).toEqual([]);

        stub.answer();
        await expect(page.locator('.agent-chat-message.assistant').filter({hasText: 'the stub answered'})).toBeVisible();
        expect(errors, errors.join(' | ')).toEqual([]);
      });
    } finally {
      await stub.close();
    }
  });

  // An answer is markdown as it is written: the list and the fence of a half-written answer are
  // already read as one, and the run is still going while they are.
  test('a streamed answer reads as markdown while it is being written', async ({page, request}) => {
    await login(page);
    const name = `e2e-markdown-${randomString(8)}`;
    await apiCreateRepo(request, {name});
    await ensureAgent(request, name);
    const repo = await repoIdOf(request, name);

    const pieces = ['Here is a list:\n\n', '- one\n', '- two\n\n', '```go\n', 'x := 1\n', '```\n'];
    const stub = await startModelStub(() => ({reasoning: 'thinking it over', content: pieces.join('')}), {hold: true});
    try {
      await withInstanceLock(async () => {
        await setRepositoryModel(page.request, stub.url);
        const session = await sessionOf(page.request, repo);
        await page.goto(`/-/agent/${session}`);
        // the page is open and follows the session; the message goes in the way the composer sends
        // it, so what this spec is about — the answer being written — is what is looked at
        const posted = await page.request.post(`/-/agent/${session}/messages`, {form: {content: 'write me a list'}, maxRedirects: 0});
        expect([200, 204, 303], await posted.text()).toContain(posted.status());
        await expect.poll(() => stub.asked.length, {message: 'the run never reached the gateway'}).toBeGreaterThan(0);

        const live = page.locator('.agent-chat-pending .agent-chat-live-text');
        stub.thought();
        await expect(page.locator('.agent-chat-thought pre')).toContainText('thinking it over');

        for (const piece of pieces.slice(0, 3)) stub.piece(piece);
        await expect(live, 'the answer is not being written into the page').toContainText('Here is a list:');
        await expect(live.locator('ul li').first()).toHaveText('one');
        // the run is still going: the send button offers to stop it, and the list above was drawn
        // while the answer was still being written
        await expect(page.locator('.agent-chat-send')).toHaveClass(/\bbusy\b/);

        for (const piece of pieces.slice(3)) stub.piece(piece);
        await expect(live).toContainText('x := 1');
        await expect(live.locator('pre').first()).toBeVisible();
        stub.answer();

        // and what the server renders is what the reader is left with
        const stored = page.locator('.agent-chat-message.assistant').filter({hasText: 'Here is a list'});
        await expect(stored).toBeVisible();
        await expect(stored.locator('ul li')).toHaveCount(2);
      });
    } finally {
      await stub.close();
    }
  });

  test('a transcript exports and pages, and an untraced run answers no trace', async ({page, request}) => {
    await login(page);
    const name = `e2e-export-${randomString(8)}`;
    await apiCreateRepo(request, {name});
    const repo = await repoIdOf(request, name);

    const stub = await startModelStub(() => ({content: 'the stub answered'}));
    try {
      await withInstanceLock(async () => {
        await setRepositoryModel(page.request, stub.url);
        const session = await sessionOf(page.request, repo);
        await page.goto(`/-/agent/${session}`);
        await page.locator('.agent-chat-input').click();
        await page.keyboard.type('say something');
        await page.locator('.agent-chat-send').click();
        await expect(page.locator('.agent-chat-message.assistant').filter({hasText: 'the stub answered'})).toBeVisible();

        // a session can decide for itself, and the toggle says which way it is set
        const autopilot = page.locator('form.agent-chat-autopilot button[name="autopilot"]');
        await expect(autopilot).toHaveAttribute('value', 'on');
        await autopilot.click();
        await expect(page.locator('form.agent-chat-autopilot button[name="autopilot"]')).toHaveAttribute('value', 'off');

        const exported = await page.request.get(`/-/agent/${session}/export`);
        expect(exported.status()).toBe(200);
        expect(exported.headers()['content-disposition']).toContain('attachment');
        expect(await exported.text()).toContain('say something');

        const empty = await page.request.get(`/-/agent/${session}/messages?before=0`);
        expect(empty.status()).toBe(200);
        expect(await empty.json()).toEqual([]);
      });
    } finally {
      await stub.close();
    }

    // the instance does not trace prompts, so every row links to a trace that is not there
    await page.goto('/-/admin/agent/runs');
    const row = page.locator('.agent-runs tbody tr[data-run-id]').first();
    const runId = await row.getAttribute('data-run-id');
    const missing = await page.request.get(`/-/admin/agent/runs/${runId}/trace`);
    expect(missing.status()).toBe(404);
  });

  test('a provider saves without headers and a key is stored', async ({page}) => {
    await login(page);
    const provider = `e2e-plain-${randomString(6)}`;
    const stored = await page.request.post('/-/admin/agent/providers/new', {
      form: {name: provider, provider: 'openai', base_url: unreachableModel, api_key_id: '', manual_models: modelId, headers: ''},
      maxRedirects: 0,
    });
    expect(stored.status(), await stored.text()).toBe(303);
    await page.goto('/-/admin/agent/providers');
    await expect(page.getByText(provider)).toBeVisible();

    const key = `e2e-key-${randomString(6)}`;
    const added = await page.request.post('/-/admin/agent/keys/new', {
      form: {name: key, value: 'e2e-value'},
      maxRedirects: 0,
    });
    expect(added.status(), await added.text()).toBe(303);
    await page.goto(`/-/admin/agent/keys?q=${key}`);
    await expect(page.getByText(key)).toBeVisible();

    for (const path of ['/-/admin/agent/groups', '/-/admin/agent/roles', '/-/admin/agent/runs', '/-/admin/agent/skills']) {
      const loaded = await page.goto(path);
      expect(loaded?.status(), path).toBe(200);
    }

    // the runs page says what the runs add up to, and the window it is read over can change
    await page.goto('/-/admin/agent/runs');
    await expect(page.locator('.agent-runs-stats')).toBeVisible();
    await expect(page.locator('.agent-runs-stats')).toContainText('Runs');
    await page.goto('/-/admin/agent/runs?days=30');
    await expect(page.locator('.agent-runs-stats')).toBeVisible();

    // a group says what it fronts, and its members are of that kind: what answers the other kind
    // is not offered at all
    await page.goto('/-/admin/agent/groups/new');
    await expect(page.locator('input[name=class]')).toHaveCount(2);
    await expect(page.locator('input[name=class][value=model]')).toBeChecked();
    expect(await page.locator('select[name=member] option[data-class=model]').count()).toBeGreaterThan(0);
    await page.locator('input[name=class][value=decision]').check();
    await expect(page.locator('select[name=member] option[data-class=model]')).toHaveCount(0);

    const listed = await (await page.request.get(`/-/admin/agent/providers?q=${provider}`)).text();
    const providerId = /\/-\/admin\/agent\/providers\/(\d+)\/edit/.exec(listed)?.[1];
    expect(providerId, 'the new provider is not on the providers page').toBeTruthy();

    const group = `e2e-group-${randomString(6)}`;
    const saved = await page.request.post('/-/admin/agent/groups/new', {
      form: {name: group, description: '', mode: 'fallback', class: 'model', member: `model:${providerId}/${modelId}`},
      maxRedirects: 0,
    });
    expect(saved.status(), await saved.text()).toBe(303);

    await page.goto(`/-/admin/agent/groups?q=${group}`);
    const row = page.locator('tbody tr', {hasText: group});
    await expect(row).toBeVisible();
    await expect(row.getByText('LLM')).toBeVisible();

    // the role that decides does not answer with a group that fronts what writes
    await page.goto('/-/admin/agent/roles');
    await expect(page.locator('select[name=role_decision] option', {hasText: group})).toHaveCount(0);
    await expect(page.locator('select[name=role_repository] option', {hasText: group})).toHaveCount(1);
  });
});

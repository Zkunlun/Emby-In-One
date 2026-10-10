// Run from the repository root: node phase8-admin-ui-contract.test.cjs
// Uses actual panel methods and the bundled Vue compiler with isolated API mocks.
// No network, backend process, Go command, deployment, or browser DOM is used.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('public/admin.js', 'utf8');
const html = fs.readFileSync('public/admin.html', 'utf8');
const checks = [];
const check = (name, run) => checks.push({ name, run });
const plain = value => JSON.parse(JSON.stringify(value));

function panel() {
  let options;
  const alerts = [];
  const store = new Map([['eio_token', 'mock-admin-token']]);
  const context = {
    Vue: { createApp(config) { options = config; return { mount() {} }; } },
    alert: text => alerts.push(text), confirm: () => true,
    lucide: { createIcons() {} },
    localStorage: {
      getItem: key => store.get(key) || null,
      setItem: (key, value) => store.set(key, value),
      removeItem: key => store.delete(key),
    },
    fetch: async () => { throw new Error('Unexpected unmocked fetch'); },
    setTimeout: () => 1, clearTimeout() {},
  };
  vm.runInNewContext(source, context, { filename: 'public/admin.js' });
  const app = options.data();
  app.$nextTick = callback => callback?.();
  for (const [key, method] of Object.entries(options.methods)) app[key] = method.bind(app);
  for (const [key, getter] of Object.entries(options.computed)) {
    Object.defineProperty(app, key, { get: getter.bind(app) });
  }
  return { app, context, alerts, store };
}

function response(status, data, length = null, invalidJSON = false) {
  return {
    status, ok: status >= 200 && status < 300,
    headers: { get: () => length },
    async json() {
      if (invalidJSON) throw new SyntaxError('Non-JSON response');
      return data;
    },
  };
}
function capacityError(code = 'UPSTREAM_CAPACITY_FULL') {
  return Object.assign(new Error('capacity conflict'), {
    status: 409, code, serverId: 'a', limit: 2, assigned: 3,
  });
}
function servers(assigned = 1) {
  return [
    { id: 'a', name: 'Server A', assignedUsers: assigned, maxConcurrent: 3, online: true },
    { id: 'b', name: 'Server B', assignedUsers: 0, maxConcurrent: 0, online: false },
  ];
}
function editingUser(h, allowed = ['a']) {
  h.app.editUserId = 'user-1';
  h.app.userForm = { username: 'alice', password: '', enabled: true, allowedServers: allowed };
  h.app.showUserModal = true;
  h.app.upstreamList = servers();
}
function noRefresh(h) {
  h.app.refreshServers = async () => {};
  h.app.refreshDashboard = async () => {};
  h.app.refreshUsers = async () => {};
}

check('finite capacity uses assigned grants', () => {
  assert.equal(panel().app.capacityUsage({ assignedUsers: 2, maxConcurrent: 3 }), '2 / 3');
});
check('unlimited capacity still displays assigned grants', () => {
  assert.equal(panel().app.capacityUsage({ assignedUsers: 5, maxConcurrent: 0 }), '5 / 不限');
});
check('missing assigned count is not invented as zero', () => {
  assert.equal(panel().app.capacityUsage({ maxConcurrent: 3 }), '未知 / 3');
});
check('server modal uses refreshed count without overwriting pending limit', () => {
  const h = panel(); h.app.upstreamList = servers(3); h.app.editServer(servers(1)[0]);
  h.app.serverForm.maxConcurrent = 2;
  assert.equal(h.app.serverAssignedUsers(), 3);
  assert.equal(h.app.serverForm.maxConcurrent, 2);
});
check('new server has no assigned users', () => {
  const h = panel(); h.app.openAddServer();
  assert.equal(h.app.serverAssignedUsers(), 0);
});

for (const code of ['UPSTREAM_CAPACITY_FULL', 'UPSTREAM_CAPACITY_BELOW_ASSIGNED']) {
  check('API retains structured ' + code, async () => {
    const h = panel();
    h.context.fetch = async () => response(409, { code, message: 'conflict', serverId: 'a', limit: 2, assigned: 3 });
    await assert.rejects(h.app.api('/mock'), e =>
      e.status === 409 && e.code === code && e.serverId === 'a' && e.limit === 2 && e.assigned === 3);
  });
}
check('empty HTTP failure cannot become false success', async () => {
  const h = panel(); h.context.fetch = async () => response(500, null, '0', true);
  await assert.rejects(h.app.api('/mock'), e => e.status === 500 && e.message === 'HTTP 500');
});
check('non-JSON error has stable HTTP fallback', async () => {
  const h = panel(); h.context.fetch = async () => response(502, null, null, true);
  await assert.rejects(h.app.api('/mock'), e => e.status === 502 && e.message === 'HTTP 502');
});
for (const status of [204, 200]) {
  check('empty success ' + status, async () => {
    const h = panel(); h.context.fetch = async () => response(status, null, '0', true);
    assert.deepEqual(plain(await h.app.api('/mock')), { success: true });
  });
}
check('401 still logs out and removes admin token', async () => {
  const h = panel(); h.app.isLoggedIn = true; h.context.fetch = async () => response(401, {});
  await assert.rejects(h.app.api('/mock'), e => e.message === 'Unauthorized');
  assert.equal(h.app.isLoggedIn, false); assert.equal(h.store.has('eio_token'), false);
});
check('API preserves successful arrays and auth header', async () => {
  const h = panel(); let headers;
  h.context.fetch = async (_, opts) => { headers = opts.headers; return response(200, servers()); };
  assert.deepEqual(plain(await h.app.api('/mock')), servers());
  assert.equal(headers['X-Emby-Token'], 'mock-admin-token');
});

for (const value of [-1, 1.5, '', null, Infinity, '3', Number.MAX_SAFE_INTEGER + 1]) {
  check('invalid capacity blocked before request: ' + String(value), async () => {
    const h = panel(); let writes = 0; h.app.openAddServer(); h.app.serverForm.maxConcurrent = value;
    h.app.api = async () => { writes++; return {}; };
    await h.app.saveServer();
    assert.equal(writes, 0); assert.equal(h.app.showModal, true); assert.equal(h.alerts.length, 1);
  });
}
for (const limit of [0, 3]) {
  check('server save submits capacity ' + limit + ' without observation field', async () => {
    const h = panel(); noRefresh(h); h.app.editServer(servers()[0]);
    h.app.serverForm.maxConcurrent = limit;
    h.app.serverForm.streamingUrlsText = 'https://one.example\nhttps://two.example';
    let payload, path, method;
    h.app.api = async (p, opts) => { path = p; method = opts.method; payload = JSON.parse(opts.body); return {}; };
    await h.app.saveServer();
    assert.equal(path, '/admin/api/upstream/a'); assert.equal(method, 'PUT');
    assert.equal(payload.maxConcurrent, limit); assert.equal('assignedUsers' in payload, false);
    assert.deepEqual(payload.streamingUrls, ['https://one.example', 'https://two.example']);
    assert.equal('streamingUrlsText' in payload, false); assert.equal(h.app.showModal, false);
  });
}
check('new server uses POST', async () => {
  const h = panel(); noRefresh(h); h.app.openAddServer(); let method;
  h.app.api = async (_, opts) => { method = opts.method; return {}; };
  await h.app.saveServer(); assert.equal(method, 'POST');
});
check('capacity-down conflict preserves server form and refreshes assigned count', async () => {
  const h = panel(); h.app.upstreamList = servers(1); h.app.editServer(servers(1)[0]);
  h.app.serverForm.maxConcurrent = 2; h.app.serverForm.name = 'pending name';
  h.app.api = async (_, opts) => {
    if (opts) throw capacityError('UPSTREAM_CAPACITY_BELOW_ASSIGNED');
    return servers(3);
  };
  await h.app.saveServer();
  assert.equal(h.app.showModal, true); assert.equal(h.app.serverForm.name, 'pending name');
  assert.equal(h.app.serverForm.maxConcurrent, 2); assert.equal(h.app.serverAssignedUsers(), 3);
  assert.match(h.alerts[0], /Server A.*不能低于.*3.*2/);
});
check('failed capacity refresh retains prior observation and pending form', async () => {
  const h = panel(); h.app.upstreamList = servers(1); h.app.editServer(servers(1)[0]);
  h.app.api = async (_, opts) => { throw opts ? capacityError() : new Error('refresh unavailable'); };
  await h.app.saveServer();
  assert.equal(h.app.showModal, true); assert.equal(h.app.upstreamList[0].assignedUsers, 1);
  assert.match(h.alerts[0], /授权容量已满/);
});
check('ordinary save error is not reported as capacity conflict', async () => {
  const h = panel(); h.app.openAddServer(); let reads = 0;
  h.app.api = async (_, opts) => { if (!opts) reads++; throw new Error('network failure'); };
  await h.app.saveServer(); assert.equal(reads, 0); assert.match(h.alerts[0], /network failure/);
});
check('user create sends explicit empty grants and true empty password', async () => {
  const h = panel(); noRefresh(h); h.app.openAddUser(); h.app.userForm.username = 'alice'; let payload;
  h.app.api = async (_, opts) => { payload = JSON.parse(opts.body); return {}; };
  await h.app.saveUser();
  assert.deepEqual(payload.allowedServers, []); assert.equal(payload.password, '');
  assert.equal(h.app.showUserModal, false);
});
check('user edit revokes all grants and omits revoked library keys', async () => {
  const h = panel(); noRefresh(h); editingUser(h, []);
  h.app.userLibraryGroups = [{ serverId: 'a', online: true, hiddenIds: ['old'] }]; let payload;
  h.app.api = async (_, opts) => { payload = JSON.parse(opts.body); return {}; };
  await h.app.saveUser();
  assert.deepEqual(payload.allowedServers, []); assert.deepEqual(payload.hiddenLibraries, {});
  assert.equal(payload.password, '');
});
check('edit preserves populated password and scopes online/offline library patch', async () => {
  const h = panel(); noRefresh(h); editingUser(h, ['a', 'b']); h.app.userForm.password = 'existing-password';
  h.app.userLibraryGroups = [
    { serverId: 'a', online: true, hiddenIds: [] },
    { serverId: 'b', online: false, hiddenIds: ['offline-old'] },
    { serverId: 'revoked', online: true, hiddenIds: ['must-not-send'] },
  ];
  let payload; h.app.api = async (_, opts) => { payload = JSON.parse(opts.body); return {}; };
  await h.app.saveUser();
  assert.deepEqual(payload.allowedServers, ['a', 'b']); assert.deepEqual(payload.hiddenLibraries, { a: [] });
  assert.equal(payload.password, 'existing-password');
});
check('grant conflict preserves user fields and refreshes counts', async () => {
  const h = panel(); editingUser(h, ['a']); h.app.userForm.password = 'pending-password';
  h.app.api = async (_, opts) => { if (opts) throw capacityError(); return servers(3); };
  await h.app.saveUser();
  assert.equal(h.app.showUserModal, true); assert.equal(h.app.userForm.password, 'pending-password');
  assert.deepEqual(plain(h.app.userForm.allowedServers), ['a']); assert.equal(h.app.upstreamList[0].assignedUsers, 3);
  assert.match(h.alerts[0], /Server A.*授权容量已满.*3.*2/);
});
check('editing zero grants performs no library fetch', async () => {
  const h = panel(); h.app.upstreamList = servers(); let reads = 0;
  h.app.api = async () => { reads++; return []; };
  await h.app.editUser({ id: 'u', username: 'alice', password: 'existing-password', enabled: true, allowedServers: [] });
  assert.equal(reads, 0); assert.deepEqual(plain(h.app.userLibraryGroups), []);
  assert.equal(h.app.userForm.password, 'existing-password');
});
check('checkbox changes library scope while retaining unsaved per-server choices', async () => {
  const h = panel(); h.app.upstreamList = servers(); const requests = [];
  h.app.api = async path => {
    requests.push(path);
    return path.includes('/a/') ? [{ id: 'a1' }, { id: 'a2' }] : [{ id: 'b1' }];
  };
  await h.app.editUser({ id: 'u', username: 'alice', password: '', enabled: true, allowedServers: ['a'], hiddenLibraries: { a: ['a1'] } });
  h.app.userLibraryGroups[0].hiddenIds = ['a2'];
  h.app.userForm.allowedServers = ['b']; await h.app.refreshUserLibraryGroups();
  assert.deepEqual(plain(h.app.userLibraryGroups.map(g => g.serverId)), ['b']);
  h.app.userForm.allowedServers = ['a']; await h.app.refreshUserLibraryGroups();
  assert.deepEqual(plain(h.app.userLibraryGroups[0].hiddenIds), ['a2']);
  assert.deepEqual(requests, ['/admin/api/upstream/a/libraries', '/admin/api/upstream/b/libraries', '/admin/api/upstream/a/libraries']);
});
check('offline authorized server retains hidden IDs but is omitted from patch', async () => {
  const h = panel(); h.app.upstreamList = servers(); h.app.api = async () => { throw new Error('offline'); };
  await h.app.editUser({ id: 'u', username: 'alice', password: '', enabled: true, allowedServers: ['b'], hiddenLibraries: { b: ['old-library'] } });
  assert.equal(h.app.userLibraryGroups[0].online, false);
  assert.deepEqual(plain(h.app.userLibraryGroups[0].hiddenIds), ['old-library']);
  assert.deepEqual(plain(h.app.groupHiddenPayload(h.app.userLibraryGroups)), {});
});
check('late old library response cannot restore revoked scope', async () => {
  const h = panel(); h.app.upstreamList = servers(); let resolveOld;
  h.app.loadLibraryGroups = async scope => scope.length ? new Promise(resolve => { resolveOld = resolve; }) : [];
  const old = h.app.editUser({ id: 'u', username: 'alice', password: '', enabled: true, allowedServers: ['a'] });
  h.app.userForm.allowedServers = []; await h.app.refreshUserLibraryGroups();
  resolveOld([{ serverId: 'a', hiddenIds: [] }]); await old;
  assert.deepEqual(plain(h.app.userLibraryGroups), []); assert.equal(h.app.libraryGroupsLoading, false);
});
check('opening new-user modal invalidates pending edit response', async () => {
  const h = panel(); h.app.upstreamList = servers(); let resolveOld;
  h.app.loadLibraryGroups = () => new Promise(resolve => { resolveOld = resolve; });
  const old = h.app.editUser({ id: 'u', username: 'alice', password: '', enabled: true, allowedServers: ['a'] });
  h.app.openAddUser(); resolveOld([{ serverId: 'a', hiddenIds: [] }]); await old;
  assert.equal(h.app.editUserId, null); assert.equal(h.app.libraryGroupsLoading, false);
  assert.deepEqual(plain(h.app.userLibraryGroups), []);
});
check('pending save cannot send an outdated library patch', async () => {
  const h = panel(); editingUser(h); h.app.libraryGroupsLoading = true; let writes = 0;
  h.app.api = async () => { writes++; return {}; }; await h.app.saveUser();
  assert.equal(writes, 0); assert.equal(h.app.showUserModal, true); assert.match(h.alerts[0], /正在加载/);
});
check('successful user save refreshes backend capacity observations', async () => {
  const h = panel(); editingUser(h); h.app.upstreamList = servers(1);
  h.app.api = async (path, opts) => opts ? {} : path.endsWith('/users') ? [] : servers(2);
  await h.app.saveUser();
  assert.equal(h.app.upstreamList[0].assignedUsers, 2); assert.equal(h.app.showUserModal, false);
});
check('user deletion refreshes capacity observations', async () => {
  const h = panel(); h.app.upstreamList = servers(2);
  h.app.api = async (path, opts) => opts ? {} : path.endsWith('/users') ? [] : servers(1);
  await h.app.deleteUser('u'); assert.equal(h.app.upstreamList[0].assignedUsers, 1);
});
check('disabling user displays backend grant count without decrementing it locally', async () => {
  const h = panel(); h.app.upstreamList = servers(2); let body;
  h.app.api = async (path, opts) => {
    if (opts) { body = JSON.parse(opts.body); return {}; }
    return path.endsWith('/users') ? [] : servers(2);
  };
  await h.app.toggleUser({ id: 'u', enabled: true });
  assert.equal(body.enabled, false); assert.equal(h.app.upstreamList[0].assignedUsers, 2);
});

// Compile the actual self-hosted template. A decoder replaces only browser DOM
// entity decoding; compilation and VNode rendering use the bundled Vue runtime.
const entities = { amp: '&', quot: '"', apos: "'", lt: '<', gt: '>', nbsp: '\u00a0' };
const decodeEntities = text => text.replace(/&(#x[\da-f]+|#\d+|amp|quot|apos|lt|gt|nbsp);/gi, (_, key) =>
  key[0] === '#' ? String.fromCodePoint(parseInt(key.slice(key[1].toLowerCase() === 'x' ? 2 : 1), key[1].toLowerCase() === 'x' ? 16 : 10)) : entities[key.toLowerCase()]);
const runtime = { console: { warn() {}, error() {} }, setTimeout, clearTimeout };
vm.runInNewContext(fs.readFileSync('public/vendor/vue.global.prod.js', 'utf8'), runtime);
const start = html.indexOf('<div id="app"');
const end = html.indexOf('<script src="vendor/vue.global.prod.js"');
assert.ok(start >= 0 && end > start, 'panel template boundaries missing');
const compileErrors = [];
const render = runtime.Vue.compile(html.slice(start, end), {
  decodeEntities, onError: error => compileErrors.push(error.message),
});
function nodes(node) {
  if (!node || typeof node !== 'object') return [];
  return [node, ...(Array.isArray(node.children) ? node.children.flatMap(nodes) : [])];
}
function textOf(node) {
  if (!node || node.type === runtime.Vue.Comment) return '';
  if (typeof node.children === 'string') return node.children;
  return Array.isArray(node.children) ? node.children.map(textOf).join(' ') : '';
}
check('bundled stylesheet supports capacity layout and disabled save feedback', () => {
  const css = fs.readFileSync('public/vendor/tailwind.css', 'utf8');
  for (const selector of ['.whitespace-nowrap', '.disabled\\:opacity-50', '.disabled\\:cursor-not-allowed']) {
    assert.ok(css.includes(selector), 'missing generated selector: ' + selector);
  }
});
check('real Vue template compiles without errors', () => {
  assert.deepEqual(compileErrors, []); assert.equal(typeof render, 'function');
});
check('rendered server page displays assigned/unlimited capacities and chosen label', () => {
  const h = panel(); h.app.isLoggedIn = true; h.app.currentPage = 'servers'; h.app.upstreamList = servers(2);
  const text = textOf(render(h.app, []));
  assert.match(text, /已授权 \/ 同播数量限制/); assert.match(text, /2 \/ 3/); assert.match(text, /0 \/ 不限/);
});
check('rendered empty user grants show no access', () => {
  const h = panel(); h.app.isLoggedIn = true; h.app.currentPage = 'users';
  h.app.userList = [{ id: 'u', username: 'alice', allowedServers: [], enabled: true, createdAt: 0 }];
  assert.match(textOf(render(h.app, [])), /无权限/);
});
check('rendered edit modal uses no-access library state and disabled pending save', () => {
  const h = panel(); h.app.isLoggedIn = true; h.app.currentPage = 'users'; editingUser(h, []);
  let tree = render(h.app, []);
  assert.match(textOf(tree), /不勾选任何服务器表示无上游访问权限/);
  assert.match(textOf(tree), /未授权任何服务器/);
  h.app.libraryGroupsLoading = true; tree = render(h.app, []);
  const save = nodes(tree).find(n => n.type === 'button' && textOf(n).trim() === '保存');
  assert.ok(save); assert.equal(save.props.disabled, true);
});


function scannerFixture({enabled = false, allowScan = false, state = 'initial_scan_pending',
  online = true, available = true, withRun = false} = {}) {
  return {
    server: {id:'src-a',name:'Example source',online},
    status: {scanEnabled:enabled, executorAvailable:available, upstreams:[{
      source:{sourceId:'src-a',allowScan,initialFullScanCompleted:state === 'idle',lastCompletedAt:''},
      state, run:withRun?{id:'run-1',sourceId:'src-a',type:'delta',state,
        pages:3,items:64,updatedAt:'2026-10-10T03:04:05Z',revision:3}:null,
      checkpoints:[{runId:'run-1',sourceId:'src-a',libraryId:'lib-a',state:'scanning',
        lastSuccessPage:1,items:64,nextStartIndex:64}],
      libraries:[{libraryId:'lib-a',initialCompleted:true,inactive:false,
        capability:'filtered',committedCursor:'2026-10-09T05:00:00Z'}],
      circuit:{state:'closed',failureCount:0,lastError:''}
    }]}
  };
}
function scannerReady(h, fixture) {
  h.app.isLoggedIn = true; h.app.currentPage = 'scanner';
  h.app.scannerStatus = fixture.status;
  h.app.scannerServers = [fixture.server];
  return fixture.status.upstreams[0];
}
check('scanner nav and passive/active warning exist', () => {
  const h = panel();
  assert.ok(h.app.pages.some(p => p.id === 'scanner' && p.name === '全库扫描'));
  assert.match(html, /与被动聚合不同/);
  assert.match(html, /风控提示/);
  assert.match(html, /Asia\/Shanghai/);
});
check('scanner defaults disabled and no active command', () => {
  const h = panel(), entry = scannerReady(h, scannerFixture());
  for(const cmd of ['start','pause','resume','stop','force_full']) assert.equal(h.app.scannerMay(entry,cmd),false);
});
check('active control requires global and source opt-in, executor and online', () => {
  for(const field of ['enabled','allowScan','online','available']) {
    const args = {enabled:true,allowScan:true,online:true,available:true};
    args[field] = false;
    const h = panel(), e = scannerReady(h, scannerFixture(args));
    assert.equal(h.app.scannerMay(e,'start'),false,field);
    assert.equal(h.app.scannerMay(e,'force_full'),false,field);
  }
  const h = panel(), e = scannerReady(h,scannerFixture({enabled:true,allowScan:true}));
  assert.equal(h.app.scannerMay(e,'start'),true);
  assert.equal(h.app.scannerMay(e,'force_full'),true);
});
check('active transitions are state-specific, no start or force full in-flight', () => {
  const args = {enabled:true,allowScan:true,withRun:true};
  for(const state of ['queued','scanning','paused_user_activity','paused_admin','paused_permission','backoff','circuit_open']) {
    const h = panel(), e = scannerReady(h,scannerFixture({...args,state}));
    assert.equal(h.app.scannerMay(e,'start'),false,state);
    assert.equal(h.app.scannerMay(e,'force_full'),false,state);
    assert.equal(h.app.scannerMay(e,'stop'),true,state);
    assert.equal(h.app.scannerMay(e,'resume'),['paused_user_activity','paused_admin','paused_permission','backoff','circuit_open'].includes(state),state);
  }
});
check('permission revoked still permits stop but not restart', () => {
  const h = panel(),e=scannerReady(h,scannerFixture({state:'paused_permission',withRun:true}));
  assert.equal(h.app.scannerMay(e,'stop'),true);
  assert.equal(h.app.scannerMay(e,'resume'),false);
  assert.equal(h.app.scannerMay(e,'pause'),false);
});
check('backoff cooldown refuses premature resume', () => {
  const h = panel(), e=scannerReady(h,scannerFixture({enabled:true,allowScan:true,state:'backoff',withRun:true}));
  e.circuit={state:'backoff',retryAt:'2999-01-01T00:00:00Z'};
  assert.equal(h.app.scannerMay(e,'resume'),false);
  e.circuit.retryAt='2020-01-01T00:00:00Z';
  assert.equal(h.app.scannerMay(e,'resume'),true);
});
check('reads scanner status and upstream names without secrets', async () => {
  const h=panel(),fixture=scannerFixture({enabled:true,allowScan:true});
  h.app.isLoggedIn=true;
  const paths=[];
  h.app.api=async path=>{paths.push(path);return path==='/admin/api/scanner/status'?fixture.status:[fixture.server];};
  await h.app.refreshScanner();
  assert.deepEqual(paths,['/admin/api/scanner/status','/admin/api/upstream']);
  assert.equal(h.app.scannerSourceName(h.app.scannerStatus.upstreams[0]),'Example source');
  assert.equal(h.app.scannerLoadError,'');
});
check('unknown shape fails closed and does not enable commands', async () => {
  const h=panel(),e=scannerReady(h,scannerFixture({enabled:true,allowScan:true}));
  h.app.api=async path=>path==='/admin/api/scanner/status'?{scanEnabled:true,upstreams:{}}:[];
  await h.app.refreshScanner();
  assert.equal(h.app.scannerStatus,null);
  assert.ok(h.app.scannerLoadError);
  assert.equal(h.app.scannerMay(e,'start'),false);
});
check('incomplete per-upstream shape fails closed before Vue rendering', async () => {
  const h=panel();scannerReady(h,scannerFixture({enabled:true,allowScan:true}));
  h.app.api=async path=>path==='/admin/api/scanner/status'
    ? {scanEnabled:true,upstreams:[{state:'scanning',source:null}]}
    : [scannerFixture().server];
  await h.app.refreshScanner();
  assert.equal(h.app.scannerStatus,null);
  assert.ok(h.app.scannerLoadError);
});
check('network error fails closed and hides stale data', async () => {
  const h=panel();scannerReady(h,scannerFixture({enabled:true,allowScan:true}));
  h.app.api=async()=>{throw new Error('token:should-not-echo');};
  await h.app.refreshScanner();
  assert.equal(h.app.scannerStatus,null);
  assert.doesNotMatch(h.app.scannerLoadError,/token|should-not-echo/);
});
check('denied confirmation issues no scanner mutation', async () => {
  const h=panel(),e=scannerReady(h,scannerFixture({enabled:true,allowScan:true}));
  h.context.confirm=()=>false;
  h.app.api=async()=>{throw new Error('command should not run');};
  await h.app.scannerCommand(e,'force_full');
  await h.app.scannerSetGlobal(false);
  await h.app.scannerSetSource(e,false);
});
check('global settings mutation explicit boolean, refresh', async () => {
  const h=panel();scannerReady(h,scannerFixture());
  const writes=[];
  h.app.api=async(path,opts)=>{
    if(opts){writes.push({path,method:opts.method,body:JSON.parse(opts.body)});return {};}
    return path==='/admin/api/scanner/status'?scannerFixture({enabled:true}).status:[scannerFixture().server];
  };
  await h.app.scannerSetGlobal(true);
  assert.deepEqual(plain(writes),[{path:'/admin/api/scanner/settings',method:'PUT',body:{scanEnabled:true}}]);
  assert.equal(h.app.scannerStatus.scanEnabled,true);
});
check('source mutation URL encodes id and submits allowScan only', async () => {
  const h=panel(),e=scannerReady(h,scannerFixture());e.source.sourceId='id with/slash';
  const writes=[];
  h.app.api=async(path,opts)=>{
    if(opts){writes.push([path,opts.method,JSON.parse(opts.body)]);return {};}
    return path==='/admin/api/scanner/status'?scannerFixture().status:[scannerFixture().server];
  };
  await h.app.scannerSetSource(e,true);
  assert.deepEqual(plain(writes),[['/admin/api/scanner/upstreams/id%20with%2Fslash','PUT',{allowScan:true}]]);
});
check('start command POST with no JSON body, refreshes status', async () => {
  const h=panel(),e=scannerReady(h,scannerFixture({enabled:true,allowScan:true}));
  const writes=[];
  h.app.api=async(path,opts)=>{
    if(opts){writes.push({path,method:opts.method,body:opts.body});return {};}
    return path==='/admin/api/scanner/status'?scannerFixture({enabled:true,allowScan:true}).status:[scannerFixture().server];
  };
  await h.app.scannerCommand(e,'start');
  assert.deepEqual(plain(writes),[{path:'/admin/api/scanner/upstreams/src-a/commands/start',method:'POST'}]);
});
check('failed mutation retains error and refreshes server truth', async () => {
  const h=panel(),e=scannerReady(h,scannerFixture({enabled:true,allowScan:true}));
  const calls=[];
  h.app.api=async(path,opts)=>{
    calls.push(path);
    if(opts) throw Object.assign(new Error('sensitive password=123'),{status:409});
    return path==='/admin/api/scanner/status'?scannerFixture({enabled:true,allowScan:true}).status:[scannerFixture().server];
  };
  await h.app.scannerCommand(e,'start');
  assert.ok(h.app.scannerActionError.includes('409'));
  assert.doesNotMatch(h.app.scannerActionError,/password/);
  assert.deepEqual(calls,['/admin/api/scanner/upstreams/src-a/commands/start','/admin/api/scanner/status','/admin/api/upstream']);
});
check('stale scanner read response cannot overwrite newer refresh', async () => {
  const h=panel();scannerReady(h,scannerFixture({enabled:true,allowScan:true}));
  let release;
  let count=0;
  h.app.api=async path=>{
    if(path==='/admin/api/upstream')return [scannerFixture().server];
    if(++count===1)return new Promise(resolve=>{release=resolve;});
    return scannerFixture({enabled:false}).status;
  };
  const slow=h.app.refreshScanner();
  await Promise.resolve();
  await h.app.refreshScanner();
  release(scannerFixture({enabled:true}).status);
  await slow;
  assert.equal(h.app.scannerStatus.scanEnabled,false);
});
check('logout revokes status even if old GET later resolves', async () => {
  const h=panel();scannerReady(h,scannerFixture({enabled:true,allowScan:true}));
  let release;
  h.app.api=async path=>path==='/admin/api/upstream'?[scannerFixture().server]:new Promise(resolve=>{release=resolve;});
  const slow=h.app.refreshScanner();
  await Promise.resolve();h.app.logout();
  release(scannerFixture({enabled:true}).status);await slow;
  assert.equal(h.app.scannerStatus,null);
  assert.equal(h.app.scannerMay(scannerFixture().status.upstreams[0],'start'),false);
});
check('scanner render shows task/checkpoint and never raw token field', () => {
  const h=panel(),e=scannerReady(h,scannerFixture({enabled:true,allowScan:true,state:'scanning',withRun:true}));
  e.source.token='secret-token';
  e.run.lastError='token=very-secret';
  const displayed=textOf(render(h.app,[]));
  assert.match(displayed,/主动全库扫描/);
  assert.match(displayed,/Example source/);
  assert.match(displayed,/媒体库进度与安全水位/);
  assert.match(displayed,/2026/);
  assert.doesNotMatch(displayed,/secret-token|very-secret/);
});
check('scanner page existing embedded stylesheet covers new utility selectors', () => {
  const css=fs.readFileSync('public/vendor/tailwind.css','utf8');
  for(const selector of ['.bg-amber-50','.text-amber-600','.disabled\\:opacity-50','.min-w-\\[700px\\]']) {
    assert.ok(css.includes(selector),'missing style '+selector);
  }
});

(async () => {
  for (const entry of checks) {
    try { await entry.run(); }
    catch (error) { console.error('FAIL: ' + entry.name + '\n' + error.stack); process.exitCode = 1; return; }
  }
  console.log(JSON.stringify({
    result: 'PASS', checks: checks.length, vue: runtime.Vue.version,
    scope: 'isolated panel methods, API mocks, actual Vue template compile/VNode render',
    goVerification: 'NOT RUN', deployment: 'NOT RUN', names: checks.map(c => c.name),
  }));
})().catch(error => { console.error(error.stack); process.exitCode = 1; });

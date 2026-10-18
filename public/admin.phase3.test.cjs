const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');

function deferred() {
  let resolve, reject;
  const promise = new Promise((a, b) => { resolve = a; reject = b; });
  return { promise, resolve, reject };
}
function fixture() {
  const alerts = [];
  const storage = new Map([['eio_token', 'fixture-admin-token']]);
  let options;
  const ctx = {
    Vue: { createApp(o) { options = o; return { mount() { return {}; } }; } },
    localStorage: {
      getItem(k) { return storage.get(k) || null; },
      setItem(k, v) { storage.set(k, v); },
      removeItem(k) { storage.delete(k); },
    },
    lucide: { createIcons() {} },
    alert(s) { alerts.push(s); },
    confirm() { return true; },
    setTimeout, clearTimeout, console,
  };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, 'admin.js'), 'utf8'), ctx);
  const ui = Object.assign(options.data(), options.methods);
  ui.$nextTick = (fn) => { if (fn) fn(); };
  ui.$refs = {};
  return { ui, alerts, storage, ctx };
}
async function settle() { await Promise.resolve(); await Promise.resolve(); }

test('one in-flight server save issues only one HTTP write', async () => {
  const { ui } = fixture();
  const d = deferred();
  let writes = 0;
  ui.editID = null; ui.editIndex = null;
  ui.serverForm = { name: 'source', maxConcurrent: 0, streamingUrlsText: '' };
  ui.api = async () => { writes++; return d.promise; };
  ui.refreshServers = async () => {}; ui.refreshDashboard = async () => {};
  const a = ui.saveServer(); const b = ui.saveServer();
  assert.equal(writes, 1);
  d.resolve({}); await Promise.all([a,b]);
  assert.equal(ui.mutationPending, false);
});

test('one in-flight delete cannot be submitted twice', async () => {
  const { ui } = fixture();
  const d = deferred(); let writes = 0;
  ui.api = async () => { writes++; return d.promise; };
  ui.refreshServers = async () => {};
  const a = ui.deleteServer('fixture-source'), b = ui.deleteServer('fixture-source');
  assert.equal(writes, 1);
  d.resolve({}); await Promise.all([a,b]);
});

test('latest users request wins when responses arrive in reverse order', async () => {
  const { ui } = fixture();
  const first=deferred(), second=deferred();
  let count=0;
  ui.api = (url) => {
    if (url === '/admin/api/users') return ++count===1 ? first.promise : second.promise;
    return Promise.resolve([]);
  };
  const a=ui.refreshUsers(), b=ui.refreshUsers();
  second.resolve([{ id:'new' }]); await settle();
  first.resolve([{ id:'stale' }]); await Promise.all([a,b]);
  assert.deepEqual(Array.from(ui.userList,v=>v.id),['new']);
});

test('temporary GET errors preserve existing server, proxy, user and log lists', async () => {
  const {ui}=fixture();
  ui.upstreamList=[{id:'source'}];ui.proxyList=[{id:'proxy'}];
  ui.userList=[{id:'user'}];ui.logs=[{level:'INFO',message:'hello'}];
  ui.api=async () => {throw new Error('temporary failure')};
  await Promise.all([ui.refreshServers(),ui.refreshProxies(),ui.refreshUsers(),ui.refreshLogs()]);
  assert.equal(ui.upstreamList.length,1); assert.equal(ui.proxyList.length,1);
  assert.equal(ui.userList.length,1);assert.equal(ui.logs.length,1);
});

test('user identity or grant changes can save while library listing is pending without overwriting hidden selection', async () => {
  const {ui}=fixture();
  ui.showUserModal=true;ui.editUserId='user1';ui.libraryGroupsLoading=true;
  ui.userForm={username:'renamed',password:'',enabled:true,allowedServers:['source1']};
  ui.userLibraryGroups=[];
  let request;
  ui.api=async (url,opts) => { if(opts?.method==='PUT') {request=JSON.parse(opts.body);return {success:true}};return []};
  ui.refreshUsers=async()=>{};
  await ui.saveUser();
  assert.equal(request.username,'renamed');
  assert.equal(request.password,undefined);
  assert.equal(Object.hasOwn(request,'hiddenLibraries'),false);
  assert.equal(ui.showUserModal,false);
});

test('editing an unchanged password does not resend it, explicit clear still sends empty password', async () => {
  const {ui}=fixture();
  ui.api=async()=>[];
  await ui.editUser({id:'user1',username:'user',password:'secret1234',enabled:true,allowedServers:[]});
  assert.equal(ui.userForm.password,'');
  assert.equal(ui.userPasswordMode,'keep');
  let writes=[];
  ui.api=async (url,opts)=>{if(opts?.method==='PUT')writes.push(JSON.parse(opts.body));return []};
  ui.refreshUsers=async()=>{};
  await ui.saveUser();
  assert.equal(Object.hasOwn(writes[0],'password'),false);
  ui.showUserModal=true;ui.userPasswordMode='clear';
  await ui.saveUser();
  assert.equal(writes[1].password,'');
});

test('proxy save and settings save share global pending guard', async () => {
  const {ui}=fixture();let writes=0;const d=deferred();
  ui.settings.adminUsername='admin';ui.adminUsernameOriginal='admin';
  ui.proxyForm={name:'p',url:'http://proxy.example:8080'};
  ui.api=async()=>{writes++;return d.promise};
  ui.refreshProxies=async()=>{};
  const a=ui.saveProxy(),b=ui.saveSettings();
  assert.equal(writes,1);
  d.resolve({});await Promise.all([a,b]);
});

test('admin logout asks server to revoke token while clearing browser token', async () => {
  const {ui,storage}=fixture();
  let requested;
  ui.api=async (path,opts)=>{requested={path,method:opts?.method};return {}};
  await ui.logout();
  assert.deepEqual(requested,{path:'/admin/api/logout',method:'POST'});
  assert.equal(storage.has('eio_token'),false);
});

test('loading home libraries cannot overwrite a newly opened modal after it closes', async () => {
  const {ui}=fixture(); const late=deferred();
  ui.upstreamList=[];
  ui.api=async (url)=> url==='/admin/api/home-libraries' ? late.promise : [];
  const opening=ui.openAdminHome();
  ui.adminHomeOpen=false;
  late.resolve({hidden:{}});
  await opening;
  assert.equal(ui.adminHomeOpen,false);
  assert.deepEqual(Array.from(ui.adminLibraryGroups),[]);
});

test('read started before successful mutation cannot revert the confirmed result', async () => {
  const { ui }=fixture();
  const stale=deferred();let reads=0;
  ui.userList=[{id:'u1',enabled:false}];
  ui.api=(url,opts)=>{
    if(opts?.method==='PUT') return Promise.resolve({success:true});
    if(url==='/admin/api/users') return ++reads===1 ? stale.promise : Promise.resolve([{id:'u1',enabled:true}]);
    return Promise.resolve([]);
  };
  const oldRead=ui.refreshUsers();
  await ui.toggleUser(ui.userList[0]);
  stale.resolve([{id:'u1',enabled:false}]);
  await oldRead;
  assert.equal(ui.userList[0].enabled,true);
});
test('fast upstream libraries display while a slower upstream still loads', async () => {
  const {ui}=fixture();const slow=deferred();
  ui.showUserModal=true;ui.editUserId='user1';
  ui.userForm={username:'user',password:'',enabled:true,allowedServers:['fast','slow']};
  ui.upstreamList=[{id:'fast',name:'Fast'},{id:'slow',name:'Slow'}];
  ui._userHiddenLibraries={fast:['lib1'],slow:['old']};
  ui.api=(url)=>url.includes('/slow/')?slow.promise:Promise.resolve([{id:'lib1',name:'Library'}]);
  const loading=ui.refreshUserLibraryGroups(); await settle();
  assert.equal(ui.userLibraryGroups.length,1);
  assert.equal(ui.userLibraryGroups[0].hiddenIds[0],'lib1');
  assert.equal(ui.libraryGroupsLoading,true);
  slow.resolve([{id:'old'}]); await loading;
  assert.equal(ui.userLibraryGroups.length,2);
  assert.equal(ui.libraryGroupsLoading,false);
});
test('explicitly edited hidden libraries cannot be silently discarded by saving during reload', async () => {
  const {ui,alerts}=fixture();let writes=0;
  ui.showUserModal=true;ui.editUserId='user1';
  ui.userLibraryDirty=true;ui.libraryGroupsLoading=true;
  ui.userForm={username:'user',password:'',enabled:true,allowedServers:['fast']};
  ui.api=async()=>{writes++;return []};
  await ui.saveUser();
  assert.equal(writes,0);
  assert.match(alerts.join(' '),/媒体库/);
});
test('offline hidden library IDs are never replaced with empty configuration', async () => {
  const {ui}=fixture();let payload;
  ui.showUserModal=true;ui.editUserId='user1';ui.libraryGroupsLoading=false;
  ui.userPasswordMode='keep';
  ui.userForm={username:'u',password:'',enabled:true,allowedServers:['offline','online']};
  ui.userLibraryGroups=[
    {serverId:'offline',online:false,hiddenIds:['old-hidden']},
    {serverId:'online',online:true,hiddenIds:['hidden2']}
  ];
  ui.api=async(url,opts)=>{if(opts?.method==='PUT')payload=JSON.parse(opts.body);return []};
  ui.refreshUsers=async()=>{};
  await ui.saveUser();
  assert.equal(Object.hasOwn(payload.hiddenLibraries,'offline'),false);
  assert.equal(payload.hiddenLibraries.online[0],'hidden2');
});
test('proxy test card suppresses repeat clicks until slow test finishes', async () => {
  const {ui}=fixture();const d=deferred();let calls=0;
  ui.upstreamList=[];
  ui.testProxyById=()=>{calls++;return d.promise;};
  const proxy={id:'proxy'};
  const a=ui.testProxyCard(proxy), b=ui.testProxyCard(proxy);
  assert.equal(calls,1);
  d.resolve({success:true,latency:12});await Promise.all([a,b]);
  assert.equal(proxy.testing,false);
});
test('login and server response errors retain existing token after transient connection failure', async () => {
  const {ui,storage}=fixture();
  ui.api=async()=>{throw new Error('network unavailable')};
  await ui.checkAuth();
  assert.equal(storage.get('eio_token'),'fixture-admin-token');
});
test('password set mode requires a new nonempty valid password, unlike explicit clear', async () => {
  const {ui,alerts}=fixture();let sends=0;
  ui.showUserModal=true;ui.editUserId='user1';
  ui.userPasswordMode='set';
  ui.userForm={username:'u',password:'short',enabled:true,allowedServers:[]};
  ui.api=async()=>{sends++;return []};
  await ui.saveUser();
  assert.equal(sends,0);assert.match(alerts.join(' '),/密码长度/);
});
test('all key mutating controls have busy-state disabled bindings', () => {
  const html=fs.readFileSync(path.join(__dirname,'admin.html'),'utf8');
  for(const click of ['saveUser','saveServer','deleteServer(s.id || s.index)','saveSettings','saveProxy','deleteProxy(p.id)','toggleUser(u)','deleteUser(u.id)','clearLogs','saveAdminHome']) {
    const index=html.indexOf('@click="'+click+'"');
    assert.notEqual(index,-1,'missing control '+click);
    assert.match(html.slice(index,index+150),/:disabled="[^"]*mutationPending/,click);
  }
  assert.match(html,/v-model="userPasswordMode"/);
  assert.match(html,/option value="clear"/);
  assert.match(html,/markUserLibrariesDirty/);
});

test('log download waits once, frees object URL and restores busy status', async () => {
  const {ui,ctx}=fixture();
  const delayed=deferred();let calls=0;let clicks=0;let revoked;
  ctx.fetch=()=>{calls++;return delayed.promise};
  ctx.document={createElement(){return {click(){clicks++}}}};
  ctx.URL={createObjectURL(){return 'blob:fixture'},revokeObjectURL(url){revoked=url}};
  const a=ui.downloadLogs(), b=ui.downloadLogs();
  assert.equal(calls,1);
  delayed.resolve({ok:true,status:200,blob:async()=>({})});
  await Promise.all([a,b]);
  assert.equal(ui.downloadingLogs,false);
  assert.equal(clicks,1);
  assert.equal(revoked,'blob:fixture');
});

test('one successful parallel read does not conceal a different failed resource', async () => {
  const {ui}=fixture();
  const later=deferred();
  ui.api=(url)=>url==='/admin/api/upstream' ? Promise.reject(new Error('source list down')) : later.promise;
  const run=ui.refreshServers();
  await settle();
  later.resolve([{id:'proxy'}]);await run;
  assert.match(ui.readError,/刷新失败/);
  ui.api=()=>Promise.resolve([]);
  await ui.refreshServers();
  assert.equal(ui.readError,'');
});
test('stale log refresh cannot end the newer loading indicator', async () => {
  const {ui}=fixture();const old=deferred(), newer=deferred();let count=0;
  ui.api=()=>++count===1?old.promise:newer.promise;
  const a=ui.refreshLogs(),b=ui.refreshLogs();
  old.resolve([{message:'old',level:'INFO'}]);await a;
  assert.equal(ui.isLoadingLogs,true);
  newer.resolve([{message:'new',level:'INFO'}]);await b;
  assert.equal(ui.isLoadingLogs,false);
  assert.equal(ui.logs[0].message,'new');
});

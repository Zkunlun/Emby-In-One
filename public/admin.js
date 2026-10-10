const { createApp } = Vue;
createApp({
  data() {
    return {
      isLoggedIn: false, loginForm: { user: '', pass: '' }, loginError: '',
      showSidebar: false,
      clientInfo: null,
      currentPage: 'dashboard', pages: [
        {id:'dashboard', name:'系统概览', icon:'layout-dashboard'},
        {id:'servers', name:'上游节点', icon:'server'},
        {id:'scanner', name:'全库扫描', icon:'scan-search'},
        {id:'users', name:'用户管理', icon:'users'},
        {id:'proxies', name:'网络代理', icon:'shield'},
        {id:'settings', name:'全局设置', icon:'settings'},
        {id:'logs', name:'运行日志', icon:'terminal'}
      ],
      stats: { upstreamCount: 0, upstreamOnline: 0, idMappings: { mappingCount: 0, persistent: true }, upstream: [] },
      upstreamList: [], proxyList: [],
      scannerStatus: null, scannerServers: [], scannerLoading: false,
      scannerLoadError: '', scannerActionError: '', scannerBusy: '',
      scannerRequestSeq: 0, scannerUpdatedAt: '',
      settings: { serverName: '', playbackMode: 'proxy', adminUsername: '', adminPassword: '', currentPassword: '', timeouts: { api: 30000, global: 15000, login: 10000, healthCheck: 10000, healthInterval: 60000, searchGracePeriod: 3000, metadataGracePeriod: 3000, latestGracePeriod: 0 } },
      adminUsernameOriginal: '',   // 服务端当前的管理员用户名，用于判断这次保存是否真的改了用户名
      logs: [], isLoadingLogs: false, saveSuccess: false,
      logLevelFilter: 'ALL', logSearch: '',
      showModal: false, editIndex: null, serverForm: {},
      showProxyModal: false, proxyForm: { name: '', url: '' },
      proxyTestState: { loading: false, result: null },
      userList: [], showUserModal: false, editUserId: null, userForm: { username: '', password: '', enabled: true, allowedServers: [] },
      userLibraryGroups: [], adminHomeOpen: false, adminLibraryGroups: [], libraryGroupsLoading: false
    };
  },
  computed: { pageTitle() { return this.pages.find(p => p.id === this.currentPage)?.name || '管理面板'; },
    dashStats() { return [
      {label:'上游服务器', val:this.stats.upstreamCount, icon:'server', bg:'bg-blue-50 text-blue-600'},
      {label:'在线节点', val:this.stats.upstreamOnline, icon:'activity', bg:'bg-green-50 text-green-600'},
      {label:'ID 映射数', val:this.stats.idMappings.mappingCount, icon:'link', bg:'bg-purple-50 text-purple-600'},
      {label:'存储引擎', val:this.stats.idMappings.persistent?'SQLite':'Memory', icon:'database', bg:'bg-orange-50 text-orange-600'}
    ];},
    filteredLogs() {
      return this.logs.filter(l => {
        if (this.logLevelFilter !== 'ALL' && l.level.toUpperCase() !== this.logLevelFilter) return false;
        if (this.logSearch && !l.message.toLowerCase().includes(this.logSearch.toLowerCase())) return false;
        return true;
      });
    }
  },
  watch: {
    currentPage(v) { if(this._refreshTimer) clearTimeout(this._refreshTimer); this._refreshTimer = setTimeout(()=>{this.refresh(); this.$nextTick(() => lucide.createIcons());}, 50); },
    'serverForm.playbackMode'(v) { if(v === 'redirect' && this.serverForm) this.serverForm.proxyId = null; }
  },
  mounted() {
    const t = localStorage.getItem('eio_token');
    if(t) this.checkAuth(t);
    this._scannerPoll = setInterval(() => {
      if(this.isLoggedIn && this.currentPage === 'scanner' && !this.scannerBusy && !this.scannerLoading) this.refreshScanner();
    }, 15000);
    this.$nextTick(() => lucide.createIcons());
  },
  beforeUnmount() { clearInterval(this._scannerPoll); clearTimeout(this._refreshTimer); },
  methods: {
    navigateTo(id) { this.currentPage = id; this.showSidebar = false; },
    async api(path, opts = {}) {
      const t = localStorage.getItem('eio_token');
      const h = { 'Content-Type': 'application/json' };
      if(t) h['X-Emby-Token'] = t;
      const r = await fetch(path, { ...opts, headers: h });
      if(r.status === 401) { this.logout(); throw new Error('Unauthorized'); }
      if(r.ok && (r.status === 204 || r.headers.get('content-length') === '0')) return { success: true };
      let data;
      try { data = await r.json(); } catch(e) { if(r.ok) throw e; data = {}; }
      if(!r.ok) {
        const error = new Error(data?.error || data?.message || ('HTTP ' + r.status));
        Object.assign(error, { status:r.status, code:data?.code, serverId:data?.serverId, limit:data?.limit, assigned:data?.assigned });
        throw error;
      }
      return data;
    },
    async checkAuth() { try { const s = await this.api('/admin/api/status'); if(!s || s.error) { this.logout(); return; } this.isLoggedIn = true; this.refresh(); this.refreshClientInfo(); } catch(e) { this.logout(); } },
    async doLogin() { try {
      const r = await fetch('/Users/AuthenticateByName', { method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify({Username:this.loginForm.user, Pw:this.loginForm.pass}) });
      if(!r.ok) throw new Error(); const data = await r.json();
      localStorage.setItem('eio_token', data.AccessToken);
      try { await this.api('/admin/api/status'); } catch(e) { localStorage.removeItem('eio_token'); this.loginError = '需要管理员权限'; return; }
      this.isLoggedIn = true; this.refresh(); this.refreshClientInfo();
    } catch(e) { this.loginError = '用户名或密码错误'; } },
    logout() {
      this.isLoggedIn = false;
      this.scannerRequestSeq++;
      this.scannerStatus = null; this.scannerServers = [];
      this.scannerLoadError = ''; this.scannerActionError = '';
      localStorage.removeItem('eio_token');
    },
    async copyClientUA() {
      if (!this.clientInfo || !this.clientInfo.userAgent) return;
      try { await navigator.clipboard.writeText(this.clientInfo.userAgent); alert('UA 已复制到剪贴板'); } catch(e) {}
    },
    refresh() {
      if(this.currentPage === 'dashboard') this.refreshDashboard();
      if(this.currentPage === 'servers') this.refreshServers();
      if(this.currentPage === 'scanner') this.refreshScanner();
      if(this.currentPage === 'users') this.refreshUsers();
      if(this.currentPage === 'proxies') this.refreshProxies();
      if(this.currentPage === 'settings') this.refreshSettings();
      if(this.currentPage === 'logs') this.refreshLogs();
    },
    scannerStateText(state) {
      return ({
        initial_scan_pending:'等待首次全量', idle:'空闲',
        queued:'排队中', scanning:'扫描中',
        paused_user_activity:'用户活跃，自动让路', paused_admin:'管理员暂停',
        paused_permission:'权限撤销，已暂停', backoff:'上游限流冷却',
        circuit_open:'上游故障熔断', completed:'完成', failed:'失败', stopped:'已停止'
      })[state] || '未知状态';
    },
    scannerTypeText(type) {
      return ({ full:'首次全量', delta:'每日增量', force_full:'强制全量校准', library_initial:'新库初始化' })[type] || '无';
    },
    scannerCircuitText(state) {
      return ({closed:'正常',open:'熔断',backoff:'限流冷却'})[state] || '未知';
    },
    scannerErrorText(code) {
      return ({
        http401:'上游认证失效（401）', http403:'上游拒绝访问（403）',
        http429:'上游限流（429）', http5xx:'上游故障（5xx）',
        permission_revoked:'扫描权限已撤销'
      })[code] || (code ? '扫描异常（请核对日志）' : '无');
    },
    scannerApiError(e) {
      if(e && e.status === 403) return '权限不足或扫描未获授权（403）';
      if(e && e.status === 404) return '扫描任务或上游已不存在（404），请刷新';
      if(e && e.status === 409) return '状态已变化或命令冲突（409），请刷新后重试';
      if(e && e.status === 503) return '上游离线、未认证或扫描暂时不可用（503）';
      if(e && e.status === 401) return '登录已失效（401）';
      return '请求失败，请检查网络或服务端日志';
    },
    scannerTime(value) {
      if(!value || typeof value !== 'string') return '—';
      const t = new Date(value);
      return Number.isNaN(t.getTime()) ? '—' : t.toLocaleString();
    },
    scannerSourceName(entry) {
      const source = entry && entry.source && entry.source.sourceId;
      const up = this.scannerServers.find(s => s.id === source);
      return up ? up.name : (source || '未知上游');
    },
    scannerSourceOnline(entry) {
      const source = entry && entry.source && entry.source.sourceId;
      const up = this.scannerServers.find(s => s.id === source);
      return !!up && up.online === true;
    },
    scannerIsActive(entry) {
      return !!entry && ['queued','scanning','paused_user_activity','paused_admin',
        'paused_permission','backoff','circuit_open'].includes(entry.state);
    },
    scannerCooldown(entry) {
      if(entry?.circuit?.state !== 'backoff') return false;
      const time = Date.parse(entry.circuit.retryAt || '');
      return !Number.isFinite(time) || time > Date.now();
    },
    scannerMay(entry, action) {
      if(!this.scannerStatus || this.scannerBusy || this.scannerLoading || !entry?.source?.sourceId) return false;
      const active = this.scannerIsActive(entry);
      if(action === 'stop') return active;
      if(action === 'pause') return active && !['paused_admin','paused_permission'].includes(entry.state);
      const canStart = this.scannerStatus.scanEnabled && entry.source.allowScan &&
        this.scannerStatus.executorAvailable === true && this.scannerSourceOnline(entry) && !this.scannerCooldown(entry);
      if(!canStart) return false;
      if(action === 'start' || action === 'force_full') return !active;
      if(action === 'resume') return active && ['paused_admin','paused_permission','paused_user_activity',
        'backoff','circuit_open'].includes(entry.state);
      return false;
    },
    async refreshScanner() {
      if(!this.isLoggedIn) return;
      const request = ++this.scannerRequestSeq;
      this.scannerLoading = true;
      this.scannerLoadError = '';
      try {
        const [status, servers] = await Promise.all([
          this.api('/admin/api/scanner/status'),
          this.api('/admin/api/upstream')
        ]);
        if(request !== this.scannerRequestSeq || !this.isLoggedIn) return;
        const validSource = item => item && item.source &&
          typeof item.source.sourceId === 'string' && item.source.sourceId.length > 0 &&
          typeof item.source.allowScan === 'boolean' && typeof item.state === 'string' &&
          Array.isArray(item.libraries) && Array.isArray(item.checkpoints) &&
          item.circuit && typeof item.circuit === 'object';
        if(!status || typeof status.scanEnabled !== 'boolean' || !Array.isArray(status.upstreams) ||
          !status.upstreams.every(validSource) || !Array.isArray(servers) ||
          !servers.every(s => s && typeof s.id === 'string' && typeof s.name === 'string' && typeof s.online === 'boolean')) {
          throw new Error('Invalid scanner status shape');
        }
        this.scannerStatus = status;
        this.scannerServers = servers;
        this.scannerUpdatedAt = new Date().toLocaleString();
      } catch(e) {
        if(request !== this.scannerRequestSeq || !this.isLoggedIn) return;
        // Fail closed: never display stale state or enable commands on fetch failure.
        this.scannerStatus = null;
        this.scannerServers = [];
        this.scannerLoadError = this.scannerApiError(e);
      } finally {
        if(request === this.scannerRequestSeq) {
          this.scannerLoading = false;
          this.$nextTick(() => lucide.createIcons());
        }
      }
    },
    async scannerWrite(key, path, body, confirmation) {
      if(!this.scannerStatus || this.scannerBusy || this.scannerLoading) return;
      if(confirmation && !confirm(confirmation)) return;
      this.scannerBusy = key;
      this.scannerActionError = '';
      this.scannerRequestSeq++; // Discard any in-flight read before mutation.
      try {
        await this.api(path, { method: 'PUT', body: JSON.stringify(body) });
      } catch(e) {
        this.scannerActionError = this.scannerApiError(e);
      } finally {
        await this.refreshScanner();
        this.scannerBusy = '';
      }
    },
    async scannerSetGlobal(value) {
      if(!this.scannerStatus || this.scannerStatus.scanEnabled === value) return;
      await this.scannerWrite('global', '/admin/api/scanner/settings', {scanEnabled:value},
        value ? '启用主动全库扫描总开关？请先确认上游允许自动请求；仍需逐上游授权。'
              : '关闭主动扫描总开关？正在运行的任务将进入权限暂停状态，需要再次手动恢复。');
    },
    async scannerSetSource(entry, value) {
      const id = entry?.source?.sourceId;
      if(!id || !this.scannerStatus || entry.source.allowScan === value) return;
      await this.scannerWrite('source:'+id, '/admin/api/scanner/upstreams/'+encodeURIComponent(id),
        {allowScan:value}, value
          ? '授权该上游进行主动全库扫描？可能造成上游风控风险；本操作不会立即开始任务。'
          : '撤销该上游的主动扫描权限？正在运行的任务将暂停。');
    },
    async scannerCommand(entry, action) {
      if(!this.scannerMay(entry, action)) return;
      const id = entry.source.sourceId;
      const messages = {
        start:'启动该上游扫描？首次为轻量全量，之后为每日增量；会请求真实上游。',
        resume:'恢复扫描任务并继续请求上游？',
        pause:'暂停该上游的扫描任务？',
        stop:'终止该上游的当前扫描任务？进度将停止推进；下次 Start 创建新任务。',
        force_full:'强制重新扫描该上游的电影/剧集轻量目录？请求量可能较大，建议低峰时执行。'
      };
      if(!confirm(messages[action])) return;
      this.scannerBusy = 'command:'+id;
      this.scannerActionError = '';
      this.scannerRequestSeq++;
      try {
        await this.api('/admin/api/scanner/upstreams/'+encodeURIComponent(id)+'/commands/'+action, {method:'POST'});
      } catch(e) {
        this.scannerActionError = this.scannerApiError(e);
      } finally {
        await this.refreshScanner();
        this.scannerBusy = '';
      }
    },
    async refreshDashboard() { try { this.stats = await this.api('/admin/api/status'); } catch(e) {} this.refreshClientInfo(); this.$nextTick(()=>lucide.createIcons()); },
    async refreshClientInfo() { try { this.clientInfo = await this.api('/admin/api/client-info'); } catch(e) {} this.$nextTick(()=>lucide.createIcons()); },
    async refreshServers() { try { this.upstreamList = await this.api('/admin/api/upstream'); } catch(e) { this.upstreamList = []; } try { await this.refreshProxies(); } catch(e) {} this.$nextTick(()=>lucide.createIcons()); },
    async refreshProxies() { try { this.proxyList = await this.api('/admin/api/proxies'); } catch(e) { this.proxyList = []; } this.$nextTick(()=>lucide.createIcons()); },
    async refreshSettings() { try { const s = await this.api('/admin/api/settings'); this.settings = { ...this.settings, ...s, adminPassword: '', currentPassword: '', timeouts: s.timeouts || this.settings.timeouts }; this.adminUsernameOriginal = s.adminUsername || ''; } catch(e) {} },
    async saveSettings() {
      const nextUsername = (this.settings.adminUsername || '').trim();
      const usernameChanged = !!nextUsername && nextUsername !== this.adminUsernameOriginal;
      const passwordChanged = !!this.settings.adminPassword;
      if ((usernameChanged || passwordChanged) && !this.settings.currentPassword) { alert('修改管理员用户名或密码时，必须输入当前密码'); return; }
      if (passwordChanged && (this.settings.adminPassword.length < 8 || this.settings.adminPassword.length > 128)) { alert('密码长度必须介于 8 和 128 之间'); return; }
      try {
        const res = await this.api('/admin/api/settings', { method:'PUT', body:JSON.stringify(this.settings) });
        this.saveSuccess = true;
        this.adminUsernameOriginal = this.settings.adminUsername || this.adminUsernameOriginal;
        this.settings.adminPassword = ''; this.settings.currentPassword = '';
        setTimeout(()=>this.saveSuccess=false,3000);
      } catch(e) { alert('保存失败：' + (e.message || '未知错误')); }
    },
    async refreshLogs() { this.isLoadingLogs = true; try { this.logs = await this.api('/admin/api/logs?limit=500'); this.$nextTick(() => { const b=this.$refs.logBox; if(b) b.scrollTop=b.scrollHeight; lucide.createIcons(); }); } catch(e) { this.logs = []; } finally { this.isLoadingLogs=false; } },
    async downloadLogs() {
      const t = localStorage.getItem('eio_token');
      const res = await fetch('/admin/api/logs/download', { headers: t ? { 'X-Emby-Token': t } : {} });
      if (!res.ok) { alert('下载失败'); return; }
      const blob = await res.blob();
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = 'emby-in-one.log';
      a.click();
      URL.revokeObjectURL(a.href);
    },
    async clearLogs() { if(!confirm('确认清空所有日志？')) return; try { await this.api('/admin/api/logs', { method:'DELETE' }); this.logs = []; } catch(e) { alert('清空失败：' + (e.message || '未知错误')); } },
    getProxyName(id) { const p = this.proxyList.find(x => x.id === id); return p ? p.name : '不使用'; },
    capacityUsage(s) {
      const assigned = Number.isInteger(s.assignedUsers) && s.assignedUsers >= 0 ? s.assignedUsers : '未知';
      const limit = s.maxConcurrent === 0 ? '不限' : s.maxConcurrent;
      return `${assigned} / ${limit}`;
    },
    serverAssignedUsers() {
      const target = this.editID ?? this.editIndex;
      if(target === null || target === undefined) return 0;
      const server = this.upstreamList.find(s => (s.id || s.index) === target);
      return server && Number.isInteger(server.assignedUsers) ? server.assignedUsers : '未知';
    },
    isCapacityConflict(e) { return e.status === 409 && ['UPSTREAM_CAPACITY_FULL','UPSTREAM_CAPACITY_BELOW_ASSIGNED'].includes(e.code); },
    capacityErrorMessage(e) {
      if(!this.isCapacityConflict(e)) return e.message || '未知错误';
      const server = this.upstreamList.find(s => s.id === e.serverId);
      const name = server?.name || e.serverId || '上游服务器';
      if(!Number.isInteger(e.assigned) || !Number.isInteger(e.limit)) return e.message || '授权容量冲突，请刷新后重试';
      if(e.code === 'UPSTREAM_CAPACITY_FULL') return `${name} 的授权容量已满（已授权 ${e.assigned} 人，上限 ${e.limit} 人）。请取消其他用户授权或提高同播数量限制。`;
      return `${name} 的同播数量限制不能低于已授权用户数（已授权 ${e.assigned} 人，提交上限 ${e.limit} 人）。请先减少授权或提高限制。`;
    },
    async refreshCapacity() { try { this.upstreamList = await this.api('/admin/api/upstream'); } catch(e) {} },
    openAddServer() { this.editID = null; this.editIndex = null; this.serverForm = { name:'', url:'', streamingUrlsText:'', authType:'password', spoofClient:'none', followRedirects:true, proxyId:null, priorityMetadata:false, maxConcurrent:0, customUserAgent:'', customClient:'', customClientVersion:'', customDeviceName:'', customDeviceId:'' }; this.showModal = true; },
    editServer(s) { this.editID = s.id || s.index; this.editIndex = s.index; this.serverForm = { ...s, maxConcurrent: s.maxConcurrent || 0, streamingUrlsText: (s.streamingUrls && s.streamingUrls.length ? s.streamingUrls : (s.streamingUrl ? [s.streamingUrl] : [])).join('\n'), customUserAgent: s.customUserAgent || '', customClient: s.customClient || '', customClientVersion: s.customClientVersion || '', customDeviceName: s.customDeviceName || '', customDeviceId: s.customDeviceId || '' }; this.showModal = true; },
    async saveServer() {
      if(!Number.isSafeInteger(this.serverForm.maxConcurrent) || this.serverForm.maxConcurrent < 0) { alert('同播数量限制必须为非负整数，0 表示不限'); return; }
      const target = this.editID !== null && this.editID !== undefined ? this.editID : this.editIndex;
      const m = target === null || target === undefined ? 'POST' : 'PUT';
      const payload = { ...this.serverForm, streamingUrls: (this.serverForm.streamingUrlsText || '').split(/[\n,]/).map(x => x.trim()).filter(x => x !== '') };
      delete payload.streamingUrlsText;
      delete payload.assignedUsers;
      try {
        const res = await this.api('/admin/api/upstream' + (target===null||target===undefined?'':'/'+target), { method:m, body:JSON.stringify(payload) });
        if (res.warning) { alert('提示：' + res.warning); }
        this.showModal = false;
        await this.refreshServers();
        await this.refreshDashboard();
      } catch (e) {
        if(this.isCapacityConflict(e)) await this.refreshCapacity();
        alert('保存失败：' + this.capacityErrorMessage(e));
      }
    },
    async deleteServer(id) { if(!confirm('删除服务器？')) return; try { await this.api('/admin/api/upstream/'+id, { method:'DELETE' }); await this.refreshServers(); } catch(e) { alert('删除失败：' + (e.message || '未知错误')); } },
    async reconnectServer(id) { try { await this.api('/admin/api/upstream/'+id+'/reconnect', { method:'POST' }); await this.refreshServers(); } catch(e) { alert('重连失败：' + (e.message || '未知错误')); } },
    async reorder(from, to) { try { await this.api('/admin/api/upstream/reorder', { method:'POST', body:JSON.stringify({fromIndex:from, toIndex:to}) }); await this.refreshServers(); } catch(e) { alert('排序失败：' + (e.message || '未知错误')); } },
    openAddProxy() { this.proxyForm = { name:'', url:'' }; this.proxyTestState = { loading: false, result: null }; this.showProxyModal = true; },
    async testProxy(proxyUrl, targetUrl) {
      try {
        const res = await this.api('/admin/api/proxies/test', { method:'POST', body:JSON.stringify({ proxyUrl, targetUrl }) });
        return res;
      } catch(e) {
        return { success: false, latency: 0, error: (e && e.message) ? e.message : '请求失败' };
      }
    },
    async testProxyById(proxyId, targetUrl) {
      try {
        const res = await this.api('/admin/api/proxies/test', { method:'POST', body:JSON.stringify({ proxyId, targetUrl }) });
        return res;
      } catch(e) {
        return { success: false, latency: 0, error: (e && e.message) ? e.message : '请求失败' };
      }
    },
    async testProxyModal() {
      if (!this.proxyForm.url) { alert('请先填写代理 URL'); return; }
      this.proxyTestState.loading = true;
      this.proxyTestState.result = null;
      const r = await this.testProxy(this.proxyForm.url, 'https://www.google.com');
      this.proxyTestState.loading = false;
      this.proxyTestState.result = r;
    },
    async testProxyCard(p) {
      p.testing = true;
      const r1 = await this.testProxyById(p.id, 'https://www.google.com');
      const bound = this.upstreamList.find(s => s.proxyId === p.id);
      let msg = r1.success ? `谷歌连通: ✓ ${r1.latency}ms` : `谷歌连通: ✗ ${r1.error || 'HTTP '+r1.statusCode}`;
      if (bound) {
        const r2 = await this.testProxyById(p.id, bound.url);
        msg += '\n' + (r2.success ? `${bound.name}: ✓ ${r2.latency}ms` : `${bound.name}: ✗ ${r2.error || 'HTTP '+r2.statusCode}`);
      }
      p.testing = false;
      alert(msg);
    },
    async saveProxy() { try { await this.api('/admin/api/proxies', { method:'POST', body:JSON.stringify(this.proxyForm) }); this.showProxyModal = false; await this.refreshProxies(); } catch(e) { alert('添加失败：' + (e.message || '未知错误')); } },
    async deleteProxy(id) { if(!confirm('删除代理？')) return; try { await this.api('/admin/api/proxies/'+id, { method:'DELETE' }); await this.refreshProxies(); } catch(e) { alert('删除失败：' + (e.message || '未知错误')); } },
    async refreshUsers() { try { this.userList = await this.api('/admin/api/users'); } catch(e) { this.userList = []; } try { this.upstreamList = await this.api('/admin/api/upstream'); } catch(e) {} this.$nextTick(()=>lucide.createIcons()); },
    openAddUser() { this._userLibraryRequest = (this._userLibraryRequest || 0) + 1; this._userHiddenLibraries = {}; this.editUserId = null; this.userForm = { username:'', password:'', enabled:true, allowedServers:[] }; this.userLibraryGroups = []; this.libraryGroupsLoading = false; this.showUserModal = true; this.$nextTick(()=>lucide.createIcons()); },
    async editUser(u) {
      this.editUserId = u.id;
      this.userForm = { username:u.username, password:u.password, enabled:u.enabled, allowedServers: u.allowedServers ? [...u.allowedServers] : [] };
      this.showUserModal = true;
      this.userLibraryGroups = [];
      this._userHiddenLibraries = { ...(u.hiddenLibraries || {}) };
      await this.refreshUserLibraryGroups();
    },
    async refreshUserLibraryGroups() {
      if(!this.editUserId || !this.showUserModal) return;
      const request = this._userLibraryRequest = (this._userLibraryRequest || 0) + 1;
      const userId = this.editUserId;
      this._userHiddenLibraries ||= {};
      for(const g of this.userLibraryGroups) this._userHiddenLibraries[g.serverId] = g.hiddenIds.slice();
      const scope = this.upstreamList.filter(s => this.userForm.allowedServers.includes(s.id));
      this.libraryGroupsLoading = true;
      try {
        const groups = await this.loadLibraryGroups(scope, this._userHiddenLibraries);
        if(request === this._userLibraryRequest && userId === this.editUserId && this.showUserModal) this.userLibraryGroups = groups;
      } finally {
        if(request === this._userLibraryRequest) this.libraryGroupsLoading = false;
        this.$nextTick(()=>lucide.createIcons());
      }
    },
    async saveUser() {
      if(this.editUserId && this.libraryGroupsLoading) { alert('媒体库列表正在加载，请稍后保存'); return; }
      try {
        if (this.editUserId) {
          const body = {};
          if (this.userForm.username) body.username = this.userForm.username;
          if (this.userForm.password !== '' && (this.userForm.password.length < 8 || this.userForm.password.length > 128)) { alert('密码长度必须介于 8 和 128 之间'); return; }
          body.password = this.userForm.password;
          body.enabled = this.userForm.enabled;
          body.allowedServers = [...this.userForm.allowedServers];
          // 离线服务器不提交 key（后端保持原配置），在线服务器提交勾选结果（含空数组 = 全部显示）。
          body.hiddenLibraries = this.groupHiddenPayload(this.userLibraryGroups.filter(g => this.userForm.allowedServers.includes(g.serverId)));
          await this.api('/admin/api/users/' + this.editUserId, { method:'PUT', body:JSON.stringify(body) });
        } else {
          if (!this.userForm.username) { alert('用户名不能为空'); return; }
          if (this.userForm.password !== '' && (this.userForm.password.length < 8 || this.userForm.password.length > 128)) { alert('密码长度必须介于 8 和 128 之间'); return; }
          const body = { username:this.userForm.username, password:this.userForm.password, allowedServers: [...this.userForm.allowedServers] };
          await this.api('/admin/api/users', { method:'POST', body:JSON.stringify(body) });
        }
        this.showUserModal = false; await this.refreshUsers();
      } catch(e) {
        if(this.isCapacityConflict(e)) await this.refreshCapacity();
        alert('保存失败：' + this.capacityErrorMessage(e));
      }
    },
    async loadLibraryGroups(servers, hidden) {
      hidden = hidden || {};
      return Promise.all(servers.map(async (s) => {
        const stored = (hidden[s.id] || []).slice();
        try {
          const libraries = await this.api('/admin/api/upstream/' + (s.id || s.index) + '/libraries');
          // 在线服务器只保留仍存在的库 ID：上游已删除的库在保存时自然清除。
          return { serverId: s.id, name: s.name, online: true, libraries: libraries, hiddenIds: stored.filter(id => libraries.some(l => l.id === id)) };
        } catch (e) {
          // 离线服务器保留已存 ID 供显示计数；保存时不提交该组 key。
          return { serverId: s.id, name: s.name, online: false, libraries: [], hiddenIds: stored };
        }
      }));
    },
    toggleGroupAll(g, val) { g.hiddenIds = val ? g.libraries.map(l => l.id) : []; },
    isGroupAllHidden(g) { return g.libraries.length > 0 && g.libraries.every(l => g.hiddenIds.includes(l.id)); },
    groupHiddenPayload(groups) {
      const payload = {};
      for (const g of groups) {
        if (!g.online) continue; // 离线组不提交 key，后端不触碰其已存配置
        payload[g.serverId] = g.hiddenIds.slice();
      }
      return payload;
    },
    async openAdminHome() {
      this.adminHomeOpen = true;
      this.adminLibraryGroups = [];
      this.libraryGroupsLoading = true;
      try {
        const hidden = (await this.api('/admin/api/home-libraries')).hidden || {};
        this.adminLibraryGroups = await this.loadLibraryGroups(this.upstreamList, hidden);
      } finally {
        this.libraryGroupsLoading = false;
        this.$nextTick(()=>lucide.createIcons());
      }
    },
    async saveAdminHome() {
      try {
        await this.api('/admin/api/home-libraries', { method:'PUT', body:JSON.stringify({ hidden: this.groupHiddenPayload(this.adminLibraryGroups) }) });
        this.adminHomeOpen = false;
      } catch(e) { alert('保存失败：' + (e.message || '未知错误')); }
    },
    async toggleUser(u) { try { const enabled = !u.enabled; await this.api('/admin/api/users/' + u.id, { method:'PUT', body:JSON.stringify({enabled}) }); await this.refreshUsers(); } catch(e) { alert('操作失败：' + (e.message || '未知错误')); } },
    async deleteUser(id) { if(!confirm('删除用户？该操作不可撤销。')) return; try { await this.api('/admin/api/users/' + id, { method:'DELETE' }); await this.refreshUsers(); } catch(e) { alert('删除失败：' + (e.message || '未知错误')); } }
  }
}).mount('#app');

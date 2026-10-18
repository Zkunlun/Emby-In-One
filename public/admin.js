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
        {id:'users', name:'用户管理', icon:'users'},
        {id:'proxies', name:'网络代理', icon:'shield'},
        {id:'settings', name:'全局设置', icon:'settings'},
        {id:'logs', name:'运行日志', icon:'terminal'}
      ],
      stats: { upstreamCount: 0, upstreamOnline: 0, idMappings: { mappingCount: 0, persistent: true }, upstream: [] },
      upstreamList: [], proxyList: [],
      settings: { serverName: '', playbackMode: 'proxy', adminUsername: '', adminPassword: '', currentPassword: '', timeouts: { api: 30000, global: 15000, login: 10000, healthCheck: 10000, healthInterval: 60000, searchGracePeriod: 3000, metadataGracePeriod: 3000, latestGracePeriod: 0 } },
      adminUsernameOriginal: '',   // 服务端当前的管理员用户名，用于判断这次保存是否真的改了用户名
      logs: [], isLoadingLogs: false, downloadingLogs: false, saveSuccess: false,
      mutationPending: false, refreshVersions: {}, readFailures: {}, readError: '', loginPending: false,
      logLevelFilter: 'ALL', logSearch: '',
      showModal: false, editIndex: null, serverForm: {},
      showProxyModal: false, proxyForm: { name: '', url: '' },
      proxyTestState: { loading: false, result: null },
      userList: [], showUserModal: false, editUserId: null, userForm: { username: '', password: '', enabled: true, allowedServers: [] },
      userLibraryGroups: [], adminHomeOpen: false, adminLibraryGroups: [], libraryGroupsLoading: false,
      adminLibraryGroupsLoading: false, userLibraryDirty: false, userPasswordMode: 'keep'
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
  mounted() { const t = localStorage.getItem('eio_token'); if(t) this.checkAuth(t); this.$nextTick(() => lucide.createIcons()); },
  methods: {
    navigateTo(id) {
      if (id !== this.currentPage) { this.readFailures = {}; this.readError = ''; }
      this.currentPage = id; this.showSidebar = false;
    },
    invalidateReads() {
      for (const key of Object.keys(this.refreshVersions)) this.refreshVersions[key]++;
    },
    async readLatest(key, path, apply) {
      const version = (this.refreshVersions[key] || 0) + 1;
      this.refreshVersions[key] = version;
      try {
        const data = await this.api(path);
        if (this.refreshVersions[key] !== version) return false;
        apply(data);
        delete this.readFailures[key];
        this.readError = Object.keys(this.readFailures).length ? '部分管理信息刷新失败；已保留现有数据，可再次刷新。' : '';
        return true;
      } catch (e) {
        if (this.refreshVersions[key] === version && e.message !== 'Unauthorized') {
          this.readFailures[key] = true;
          this.readError = '部分管理信息刷新失败；已保留现有数据，可再次刷新。';
        }
        return false;
      }
    },
    async runMutation(task) {
      if (this.mutationPending) return;
      this.mutationPending = true;
      this.invalidateReads();
      try { return await task(); }
      finally { this.mutationPending = false; }
    },
    endSession() {
      this.isLoggedIn = false;
      localStorage.removeItem('eio_token');
      this.invalidateReads();
      this.readFailures = {}; this.readError = '';
    },
    async api(path, opts = {}) {
      const t = localStorage.getItem('eio_token');
      const h = { 'Content-Type': 'application/json' };
      if(t) h['X-Emby-Token'] = t;
      const r = await fetch(path, { ...opts, headers: h });
      if(r.status === 401) { this.endSession(); throw new Error('Unauthorized'); }
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
    async checkAuth() {
      try {
        const s = await this.api('/admin/api/status');
        if (!s || s.error) { this.endSession(); return; }
        this.isLoggedIn = true; this.refresh(); this.refreshClientInfo();
      } catch (e) {
        if (e.message === 'Unauthorized') this.endSession();
        else this.loginError = '暂时无法验证管理会话，请检查网络后重试';
      }
    },
    async doLogin() {
      if (this.loginPending) return;
      this.loginPending = true;
      try {
        const r = await fetch('/Users/AuthenticateByName', { method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify({Username:this.loginForm.user, Pw:this.loginForm.pass}) });
        if(!r.ok) throw new Error();
        const data = await r.json();
        localStorage.setItem('eio_token', data.AccessToken);
        try { await this.api('/admin/api/status'); }
        catch(e) { localStorage.removeItem('eio_token'); this.loginError = '需要管理员权限'; return; }
        this.isLoggedIn = true; this.loginError = '';
        this.refresh(); this.refreshClientInfo();
      } catch(e) { this.loginError = '用户名或密码错误'; }
      finally { this.loginPending = false; }
    },
    async logout() {
      // Issue the revocation request before clearing the local token. Session
      // state clears immediately; a failed server request cannot trap the UI.
      const request = this.api('/admin/api/logout', { method:'POST' }).catch(() => {});
      this.endSession();
      await request;
    },
    async copyClientUA() {
      if (!this.clientInfo || !this.clientInfo.userAgent) return;
      try { await navigator.clipboard.writeText(this.clientInfo.userAgent); alert('UA 已复制到剪贴板'); } catch(e) {}
    },
    refresh() {
      if(this.currentPage === 'dashboard') this.refreshDashboard();
      if(this.currentPage === 'servers') this.refreshServers();
      if(this.currentPage === 'users') this.refreshUsers();
      if(this.currentPage === 'proxies') this.refreshProxies();
      if(this.currentPage === 'settings') this.refreshSettings();
      if(this.currentPage === 'logs') this.refreshLogs();
    },
    async refreshDashboard() {
      await this.readLatest('status','/admin/api/status', s => { this.stats = s; });
      this.refreshClientInfo(); this.$nextTick(()=>lucide.createIcons());
    },
    async refreshClientInfo() {
      await this.readLatest('client-info','/admin/api/client-info', s => { this.clientInfo = s; });
      this.$nextTick(()=>lucide.createIcons());
    },
    async refreshServers() {
      await Promise.all([
        this.readLatest('upstream','/admin/api/upstream', s => { this.upstreamList = s; }),
        this.refreshProxies()
      ]);
      this.$nextTick(()=>lucide.createIcons());
    },
    async refreshProxies() {
      await this.readLatest('proxies','/admin/api/proxies', p => { this.proxyList = p; });
      this.$nextTick(()=>lucide.createIcons());
    },
    async refreshSettings() {
      await this.readLatest('settings','/admin/api/settings', s => {
        this.settings = { ...this.settings, ...s, adminPassword: '', currentPassword: '', timeouts: s.timeouts || this.settings.timeouts };
        this.adminUsernameOriginal = s.adminUsername || '';
      });
    },
    async saveSettings() { return this.runMutation(async () => {
      const nextUsername = (this.settings.adminUsername || '').trim();
      const usernameChanged = !!nextUsername && nextUsername !== this.adminUsernameOriginal;
      const passwordChanged = !!this.settings.adminPassword;
      if ((usernameChanged || passwordChanged) && !this.settings.currentPassword) { alert('修改管理员用户名或密码时，必须输入当前密码'); return; }
      if (passwordChanged && (this.settings.adminPassword.length < 8 || this.settings.adminPassword.length > 128)) { alert('密码长度必须介于 8 和 128 之间'); return; }
      try {
        await this.api('/admin/api/settings', { method:'PUT', body:JSON.stringify(this.settings) });
        this.saveSuccess = true;
        this.adminUsernameOriginal = this.settings.adminUsername || this.adminUsernameOriginal;
        this.settings.adminPassword = ''; this.settings.currentPassword = '';
        setTimeout(()=>this.saveSuccess=false,3000);
      } catch(e) { alert('保存失败：' + (e.message || '未知错误')); }
    }); },
    async refreshLogs() {
      const request = this._logsLoadingRequest = (this._logsLoadingRequest || 0) + 1;
      this.isLoadingLogs = true;
      await this.readLatest('logs','/admin/api/logs?limit=500', entries => {
        this.logs = entries;
        this.$nextTick(() => { const b=this.$refs.logBox; if(b) b.scrollTop=b.scrollHeight; lucide.createIcons(); });
      });
      if (request === this._logsLoadingRequest) this.isLoadingLogs = false;
    },
    async downloadLogs() {
      if (this.downloadingLogs) return;
      this.downloadingLogs = true;
      let objectUrl;
      try {
        const t = localStorage.getItem('eio_token');
        const res = await fetch('/admin/api/logs/download', { headers: t ? { 'X-Emby-Token': t } : {} });
        if (res.status === 401) { this.endSession(); return; }
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const blob = await res.blob();
        const a = document.createElement('a');
        objectUrl = URL.createObjectURL(blob);
        a.href = objectUrl;
        a.download = 'emby-in-one.log';
        a.click();
      } catch(e) { alert('下载失败：' + (e.message || '网络异常')); }
      finally { if (objectUrl) URL.revokeObjectURL(objectUrl); this.downloadingLogs = false; }
    },
    async clearLogs() { if(!confirm('确认清空所有日志？')) return; return this.runMutation(async () => { try { await this.api('/admin/api/logs', { method:'DELETE' }); this.invalidateReads(); this.logs = []; } catch(e) { alert('清空失败：' + (e.message || '未知错误')); } }); },
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
    async refreshCapacity() { await this.readLatest('upstream','/admin/api/upstream', s => { this.upstreamList = s; }); },
    openAddServer() { this.editID = null; this.editIndex = null; this.serverForm = { name:'', url:'', streamingUrlsText:'', authType:'password', spoofClient:'none', followRedirects:true, proxyId:null, priorityMetadata:false, maxConcurrent:0, customUserAgent:'', customClient:'', customClientVersion:'', customDeviceName:'', customDeviceId:'' }; this.showModal = true; },
    editServer(s) { this.editID = s.id || s.index; this.editIndex = s.index; this.serverForm = { ...s, maxConcurrent: s.maxConcurrent || 0, streamingUrlsText: (s.streamingUrls && s.streamingUrls.length ? s.streamingUrls : (s.streamingUrl ? [s.streamingUrl] : [])).join('\n'), customUserAgent: s.customUserAgent || '', customClient: s.customClient || '', customClientVersion: s.customClientVersion || '', customDeviceName: s.customDeviceName || '', customDeviceId: s.customDeviceId || '' }; this.showModal = true; },
    async saveServer() { return this.runMutation(async () => {
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
        await Promise.all([this.refreshServers(), this.refreshDashboard()]);
      } catch (e) {
        if(this.isCapacityConflict(e)) await this.refreshCapacity();
        alert('保存失败：' + this.capacityErrorMessage(e));
      }
    }); },
    async deleteServer(id) { if(this.mutationPending || !confirm('删除服务器？')) return; return this.runMutation(async () => {
      try {
        await this.api('/admin/api/upstream/'+id, { method:'DELETE' });
        this.upstreamList = this.upstreamList.filter(s => (s.id || s.index) !== id);
        await Promise.all([this.refreshServers(),this.refreshDashboard()]);
      } catch(e) { alert('删除失败：' + (e.message || '未知错误')); }
    }); },
    async reconnectServer(id) { return this.runMutation(async () => {
      try { await this.api('/admin/api/upstream/'+id+'/reconnect', { method:'POST' }); await this.refreshServers(); }
      catch(e) { alert('重连失败：' + (e.message || '未知错误')); }
    }); },
    async reorder(from, to) { return this.runMutation(async () => {
      try {
        await this.api('/admin/api/upstream/reorder', { method:'POST', body:JSON.stringify({fromIndex:from, toIndex:to}) });
        const list = this.upstreamList.slice();
        if(from>=0 && to>=0 && from<list.length && to<list.length) {
          list.splice(to,0,list.splice(from,1)[0]); this.upstreamList = list;
        }
        await this.refreshServers();
      } catch(e) { alert('排序失败：' + (e.message || '未知错误')); }
    }); },
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
      if(this.proxyTestState.loading) return;
      if (!this.proxyForm.url) { alert('请先填写代理 URL'); return; }
      this.proxyTestState.loading = true;
      this.proxyTestState.result = null;
      try { this.proxyTestState.result = await this.testProxy(this.proxyForm.url, 'https://www.google.com'); }
      finally { this.proxyTestState.loading = false; }
    },
    async testProxyCard(p) {
      if(p.testing) return;
      p.testing = true;
      try {
      const r1 = await this.testProxyById(p.id, 'https://www.google.com');
      const bound = this.upstreamList.find(s => s.proxyId === p.id);
      let msg = r1.success ? `谷歌连通: ✓ ${r1.latency}ms` : `谷歌连通: ✗ ${r1.error || 'HTTP '+r1.statusCode}`;
      if (bound) {
        const r2 = await this.testProxyById(p.id, bound.url);
        msg += '\n' + (r2.success ? `${bound.name}: ✓ ${r2.latency}ms` : `${bound.name}: ✗ ${r2.error || 'HTTP '+r2.statusCode}`);
      }
        alert(msg);
      } finally { p.testing = false; }
    },
    async saveProxy() { return this.runMutation(async () => {
      try { await this.api('/admin/api/proxies', { method:'POST', body:JSON.stringify(this.proxyForm) }); this.showProxyModal = false; await this.refreshProxies(); }
      catch(e) { alert('添加失败：' + (e.message || '未知错误')); }
    }); },
    async deleteProxy(id) { if(this.mutationPending || !confirm('删除代理？')) return; return this.runMutation(async () => {
      try { await this.api('/admin/api/proxies/'+id, { method:'DELETE' }); this.proxyList = this.proxyList.filter(p => p.id!==id); await this.refreshProxies(); }
      catch(e) { alert('删除失败：' + (e.message || '未知错误')); }
    }); },
    async refreshUsers() {
      await Promise.all([
        this.readLatest('users','/admin/api/users', u => { this.userList = u; }),
        this.readLatest('upstream','/admin/api/upstream', s => { this.upstreamList = s; })
      ]);
      this.$nextTick(()=>lucide.createIcons());
    },
    openAddUser() { this._userLibraryRequest = (this._userLibraryRequest || 0) + 1; this._userHiddenLibraries = {}; this.editUserId = null; this.userPasswordMode = 'set'; this.userLibraryDirty = false; this.userForm = { username:'', password:'', enabled:true, allowedServers:[] }; this.userLibraryGroups = []; this.libraryGroupsLoading = false; this.showUserModal = true; this.$nextTick(()=>lucide.createIcons()); },
    async editUser(u) {
      this.editUserId = u.id;
      this.userForm = { username:u.username, password:'', enabled:u.enabled, allowedServers: u.allowedServers ? [...u.allowedServers] : [] };
      this.userPasswordMode = 'keep';
      this.userLibraryDirty = false;
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
        const valid = () => request === this._userLibraryRequest && userId === this.editUserId && this.showUserModal;
        const groups = await this.loadLibraryGroups(scope, this._userHiddenLibraries, partial => {
          if(valid()) this.userLibraryGroups = partial;
        });
        if(valid()) this.userLibraryGroups = groups;
      } finally {
        if(request === this._userLibraryRequest) this.libraryGroupsLoading = false;
        this.$nextTick(()=>lucide.createIcons());
      }
    },
    async saveUser() { return this.runMutation(async () => {
      if(this.editUserId && this.libraryGroupsLoading && this.userLibraryDirty) {
        alert('媒体库显示设置仍在加载，请等待完成后保存，避免丢失勾选修改');
        return;
      }
      try {
        if (this.editUserId) {
          const body = {};
          if (this.userForm.username) body.username = this.userForm.username;
          if (this.userPasswordMode === 'set') {
            if (this.userForm.password.length < 8 || this.userForm.password.length > 128) { alert('密码长度必须介于 8 和 128 之间'); return; }
            body.password = this.userForm.password;
          } else if (this.userPasswordMode === 'clear') {
            body.password = '';
          }
          body.enabled = this.userForm.enabled;
          body.allowedServers = [...this.userForm.allowedServers];
          // 离线服务器不提交 key（后端保持原配置），在线服务器提交勾选结果（含空数组 = 全部显示）。
          if (!this.libraryGroupsLoading) {
            body.hiddenLibraries = this.groupHiddenPayload(this.userLibraryGroups.filter(g => this.userForm.allowedServers.includes(g.serverId)));
          }
          await this.api('/admin/api/users/' + this.editUserId, { method:'PUT', body:JSON.stringify(body) });
        } else {
          if (!this.userForm.username) { alert('用户名不能为空'); return; }
          if (this.userForm.password !== '' && (this.userForm.password.length < 8 || this.userForm.password.length > 128)) { alert('密码长度必须介于 8 和 128 之间'); return; }
          const body = { username:this.userForm.username, password:this.userForm.password, allowedServers: [...this.userForm.allowedServers] };
          await this.api('/admin/api/users', { method:'POST', body:JSON.stringify(body) });
        }
        this._userLibraryRequest = (this._userLibraryRequest || 0) + 1;
        this.showUserModal = false; await this.refreshUsers();
      } catch(e) {
        if(this.isCapacityConflict(e)) await this.refreshCapacity();
        alert('保存失败：' + this.capacityErrorMessage(e));
      }
    }); },
    async loadLibraryGroups(servers, hidden, onPartial) {
      hidden = hidden || {};
      const groups = new Array(servers.length);
      await Promise.all(servers.map(async (s, i) => {
        const stored = (hidden[s.id] || []).slice();
        try {
          const libraries = await this.api('/admin/api/upstream/' + (s.id || s.index) + '/libraries');
          // 在线服务器只保留仍存在的库 ID：上游已删除的库在保存时自然清除。
          groups[i] = { serverId: s.id, name: s.name, online: true, libraries: libraries, hiddenIds: stored.filter(id => libraries.some(l => l.id === id)) };
        } catch (e) {
          // 离线服务器保留已存 ID 供显示计数；保存时不提交该组 key。
          groups[i] = { serverId: s.id, name: s.name, online: false, libraries: [], hiddenIds: stored };
        }
        if (onPartial) onPartial(groups.filter(Boolean));
      }));
      return groups;
    },
    markUserLibrariesDirty() { this.userLibraryDirty = true; },
    toggleGroupAll(g, val) { g.hiddenIds = val ? g.libraries.map(l => l.id) : []; if (this.userLibraryGroups.includes(g)) this.userLibraryDirty = true; },
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
      const request = this._adminHomeRequest = (this._adminHomeRequest || 0) + 1;
      this.adminLibraryGroupsLoading = true;
      try {
        const hidden = (await this.api('/admin/api/home-libraries')).hidden || {};
        const valid = () => request === this._adminHomeRequest && this.adminHomeOpen;
        if (!valid()) return;
        const groups = await this.loadLibraryGroups(this.upstreamList, hidden, partial => {
          if (valid()) this.adminLibraryGroups = partial;
        });
        if (valid()) this.adminLibraryGroups = groups;
      } catch(e) {
        if (this.adminHomeOpen && request === this._adminHomeRequest) alert('读取首页媒体库失败：' + e.message);
      } finally {
        if (request === this._adminHomeRequest) this.adminLibraryGroupsLoading = false;
        this.$nextTick(()=>lucide.createIcons());
      }
    },
    async saveAdminHome() { if(this.adminLibraryGroupsLoading) return;
      return this.runMutation(async () => {
        try {
          await this.api('/admin/api/home-libraries', { method:'PUT', body:JSON.stringify({ hidden: this.groupHiddenPayload(this.adminLibraryGroups) }) });
          this._adminHomeRequest = (this._adminHomeRequest || 0) + 1;
          this.adminHomeOpen = false;
        } catch(e) { alert('保存失败：' + (e.message || '未知错误')); }
      });
    },
    async toggleUser(u) { return this.runMutation(async () => {
      try { const enabled = !u.enabled; await this.api('/admin/api/users/' + u.id, { method:'PUT', body:JSON.stringify({enabled}) }); u.enabled = enabled; await this.refreshUsers(); }
      catch(e) { alert('操作失败：' + (e.message || '未知错误')); }
    }); },
    async deleteUser(id) { if(this.mutationPending || !confirm('删除用户？该操作不可撤销。')) return; return this.runMutation(async () => {
      try { await this.api('/admin/api/users/' + id, { method:'DELETE' }); this.userList = this.userList.filter(u => u.id!==id); await this.refreshUsers(); }
      catch(e) { alert('删除失败：' + (e.message || '未知错误')); }
    }); }
  }
}).mount('#app');

/* 历史会话前端逻辑（原生 JS，无依赖）。
 * 后端约定见 0/webauth.go：
 *   /hybapi/login|logout|me|models|sen|sen/:id/messages|upload|file/:id|chat/completions
 * 聊天走 /hybapi/chat/completions（SSE 直通记录代理），响应头 X-Sen-Id 返回会话 id，
 * 后续轮次带上 X-Sen-Id 让同一会话持续追加。 */
(function () {
	'use strict';

	var $ = function (id) { return document.getElementById(id); };

	var LS_TOKEN = 'hyb_token';
	var LS_MODEL = 'hyb_model';
	var LS_SYSP = 'hyb_sysp';
	var LS_FILT = 'hyb_filters2'; // 835: 升版一次性重置(旧键可能存有 re:true / cs:true 旧默认)

	var state = {
		token: localStorage.getItem(LS_TOKEN) || '',
		user: null,
		models: [],
		senList: [],
		senId: 0,
		msgs: [],
		includeKey: new URLSearchParams(location.search).get('senKey') === '1',
		model: localStorage.getItem(LS_MODEL) || '',
		sysp: localStorage.getItem(LS_SYSP) || '',
		files: [],      // 待发送附件 {kind:'image'|'text', name, size, type, dataUrl?, text?}
		streaming: false,
		aborter: null,
		stick: true,    // 自动滚动开关
		filters: null,  // 71/835: 搜索选项 {q,a,date,cs}
		hl: null,       // 72: 高亮状态 {marks, pos}
		hlCtx: null,    // 72/836: 打开会话时携带的 {terms,cs,scope}
	};
	state.filters = (function () {
		var f = null;
		try { f = JSON.parse(localStorage.getItem(LS_FILT)); } catch (e) { /* ignore */ }
		return Object.assign({ q: true, a: true, date: false, cs: false }, f || {});
	})();

	/* ---------------- 通用 ---------------- */

	function toast(msg) {
		var t = $('toast');
		t.textContent = msg;
		t.classList.remove('hidden');
		clearTimeout(toast._t);
		toast._t = setTimeout(function () { t.classList.add('hidden'); }, 2600);
	}

	/* ---------------- 自定义 tooltip (837) ---------------- */
	// 通用富文本提示框：任何元素写 data-tooltip 即生效，替代原生 title
	// (title 无法渲染 <b>问答</b> 这类富文本，且两者并存会双提示框)。
	// 事件委托到 document：mouseover/mouseout 冒泡阶段找 closest('[data-tooltip]')，
	// focusin/focusout 让键盘聚焦也能看到；约 250ms 延迟防鼠标扫过时闪烁；
	// 浮层 position:fixed 挂在 body 下(不被侧栏 overflow 裁剪)，贴边收进视口、
	// 底部放不下自动翻到上方；移出/失焦/滚动即收起。内容只来自开发者写死的
	// 属性值(无用户输入)，innerHTML 渲染无注入风险。
	var tipEl = null, tipTimer = 0, tipFor = null;

	function tipShow(target) {
		if (tipFor === target) return;
		tipHide();
		tipFor = target;
		tipTimer = setTimeout(function () {
			var html = target.getAttribute('data-tooltip');
			if (!html) return;
			if (!tipEl) {
				tipEl = el('div', 'ctip');
				document.body.appendChild(tipEl);
			}
			tipEl.innerHTML = html;
			tipEl.classList.add('show');
			tipEl.style.left = '0px';
			tipEl.style.top = '0px'; // 先复位再量尺寸
			var r = target.getBoundingClientRect();
			var tw = tipEl.offsetWidth, th = tipEl.offsetHeight;
			var vw = document.documentElement.clientWidth, vh = document.documentElement.clientHeight;
			var x = Math.min(Math.max(8, r.left), Math.max(8, vw - tw - 8));
			var y = r.bottom + 8;
			if (y + th > vh - 8) y = Math.max(8, r.top - th - 8);
			tipEl.style.left = x + 'px';
			tipEl.style.top = y + 'px';
		}, 250);
	}

	function tipHide() {
		clearTimeout(tipTimer);
		tipTimer = 0;
		tipFor = null;
		if (tipEl) tipEl.classList.remove('show');
	}

	document.addEventListener('mouseover', function (e) {
		var t = e.target && e.target.closest ? e.target.closest('[data-tooltip]') : null;
		if (t) tipShow(t);
		else tipHide();
	});
	document.addEventListener('mouseout', function (e) {
		var t = e.target && e.target.closest ? e.target.closest('[data-tooltip]') : null;
		if (!t) return;
		if (e.relatedTarget && t.contains(e.relatedTarget)) return; // 仍在控件内部移动
		tipHide();
	});
	document.addEventListener('focusin', function (e) {
		var t = e.target && e.target.closest ? e.target.closest('[data-tooltip]') : null;
		if (t) tipShow(t);
	});
	document.addEventListener('focusout', tipHide);
	window.addEventListener('scroll', tipHide, true); // 滚动后浮层会脱离锚点，直接收起

	function fmtSize(n) {
		if (n >= 1048576) return (n / 1048576).toFixed(1) + ' MB';
		if (n >= 1024) return (n / 1024).toFixed(1) + ' KB';
		return n + ' B';
	}

	// 周一~周日 → ①②③④⑤⑥⑦（getDay(): 0=周日）
	var WEEK_CHAR = ['⑦', '①', '②', '③', '④', '⑤', '⑥'];

	function tsToDate(ts) {
		if (!ts) return '';
		var d = new Date(ts > 1e12 ? ts : ts * 1000);
		var p = function (x) { return (x < 10 ? '0' : '') + x; };
		return String(d.getFullYear()).slice(2) + '/' + p(d.getMonth() + 1) + '/' + p(d.getDate());
	}

	/* ---- 831/832/833: 日期选择恢复原生控件(透明覆盖层)，显示一律 26/08/15，默认空白 ---- */

	// normDateDigits: 去掉非数字后解析并校验真实日历日(6位=yyMMdd 补20，8位=yyyyMMdd)
	function normDateDigits(str) {
		var d = String(str || '').replace(/\D+/g, '');
		if (!d) return { ok: true, text: '' };
		var y, m, dd;
		if (d.length === 6) { y = 2000 + (+d.slice(0, 2)); m = +d.slice(2, 4); dd = +d.slice(4, 6); }
		else if (d.length === 8) { y = +d.slice(0, 4); m = +d.slice(4, 6); dd = +d.slice(6, 8); }
		else return { ok: false, text: '' };
		var dt = new Date(y, m - 1, dd);
		if (dt.getFullYear() !== y || dt.getMonth() !== m - 1 || dt.getDate() !== dd) return { ok: false, text: '' };
		var p = function (x) { return (x < 10 ? '0' : '') + x; };
		return { ok: true, text: String(y).slice(2) + '/' + p(m) + '/' + p(dd), iso: y + '-' + p(m) + '-' + p(dd) };
	}

	// dateInputISO: 输入框值(原生 date 为 yyyy-MM-dd) → 后端参数 yyyy-MM-dd；无效/为空返回 ''
	function dateInputISO(val) {
		var n = normDateDigits(val);
		return n.ok ? (n.iso || '') : '';
	}

	// fmtISOToDate: '2026-08-14' → 显示用 '26/08/14'
	function fmtISOToDate(iso) {
		if (!iso) return '';
		var p = String(iso).split('-');
		if (p.length !== 3) return '';
		return p[0].slice(2) + '/' + p[1] + '/' + p[2];
	}

	// wireDateInput: 透明原生 date 覆盖在显示层上，点按即弹系统日期控件(桌面端再补调
	// showPicker()；移动端点按原生输入框本身就会弹)。选中后显示层联动刷新列表；
	// 另一侧为空时镜像同值(最常用的"单日"筛选选一次即成)；✕ 清除该侧。
	function wireDateInput(id) {
		var inp = $(id);
		var wrap = $(id + '-wrap');
		var show = $(id + '-show');
		var clearBtn = $(id + '-clear');
		var refresh = function () {
			show.textContent = fmtISOToDate(inp.value);
			clearBtn.classList.toggle('hidden', !inp.value);
		};
		wrap.addEventListener('click', function (e) {
			if (clearBtn.contains(e.target)) return;
			try { if (typeof inp.showPicker === 'function') inp.showPicker(); } catch (err) { /* 旧浏览器: 聚焦即可 */ }
		});
		inp.addEventListener('change', function () {
			refresh();
			var otherId = id === 'date-from' ? 'date-to' : 'date-from';
			var other = $(otherId);
			if (inp.value && !other.value) {
				other.value = inp.value;
				$(otherId + '-show').textContent = fmtISOToDate(other.value);
				$(otherId + '-clear').classList.toggle('hidden', !other.value);
			}
			loadSenList();
		});
		clearBtn.addEventListener('click', function (e) {
			e.preventDefault();
			e.stopPropagation();
			inp.value = '';
			refresh();
			loadSenList();
		});
		refresh();
	}

	function fmtTime(ts) {
		if (!ts) return '';
		var ms = ts > 1e12 ? ts : ts * 1000; // 新数据为毫秒，旧行为秒
		var d = new Date(ms);
		var p = function (x) { return (x < 10 ? '0' : '') + x; };
		var yy = String(d.getFullYear()).slice(2);
		return yy + '/' + p(d.getMonth() + 1) + '/' + p(d.getDate()) + WEEK_CHAR[d.getDay()] + p(d.getHours()) + ':' + p(d.getMinutes());
	}

	// 头像字母取"模型名"的首字母：a_model 为 渠道/模型（取 / 后段），
	// 如 gd-llm/GD-LLM → G，moonshot-v1-8k → M。
	function modelLetter(model) {
		var m = String(model || '').trim();
		if (!m) return '?';
		var i = m.lastIndexOf('/');
		if (i >= 0) m = m.slice(i + 1).trim();
		return m ? m.charAt(0).toUpperCase() : '?';
	}

	// 35: 模型显示名(渠道名/模型名)，供欢迎页与流式消息头使用
	function modelLabel(id) {
		for (var i = 0; i < state.models.length; i++) {
			if (state.models[i].id === id) return state.models[i].name || id;
		}
		return id;
	}

	/* ---- 72: 全篇高亮 + 导航条(n/total ↑↓ ✕)；834/835: 多条件全部高亮 ---- */

	// parseTerms 与服务端 parseSearchTerms 同规则(835 普通用户语法)：
	// 空白分隔多条件(AND)；每个条件 [-] [问:|答:] (引号短语 | 裸词[|裸词...])。
	// - 前缀=排除(高亮时跳过)；问:/答: 限定字段；裸词内 | 分隔候选；引号内全是字面。
	function parseTerms(q) {
		var terms = [], i = 0, n = q.length;
		while (i < n) {
			while (i < n && /\s/.test(q[i])) i++;
			if (i >= n) break;
			var t = { negate: false, field: 0, alts: [] };
			if (q[i] === '-') { t.negate = true; i++; }
			if (q.substr(i, 2) === '问:') { t.field = 'q'; i += 2; }
			else if (q.substr(i, 2) === '答:') { t.field = 'a'; i += 2; }
			var text = '', quoted = false;
			if (q[i] === '"') {
				quoted = true;
				i++;
				while (i < n && q[i] !== '"') text += q[i++];
				if (i < n) i++; // 收掉闭引号
			} else {
				while (i < n && !/\s/.test(q[i]) && q[i] !== '"') text += q[i++];
			}
			if (!text) continue; // 裸 - / 问: / ""
			if (quoted) {
				t.alts = [text];
			} else {
				text.split('|').forEach(function (p) { if (p) t.alts.push(p); });
				if (!t.alts.length) continue;
			}
			terms.push(t);
		}
		return terms;
	}

	// clearHighlight 移除所有高亮标记并隐藏导航条；clearCtx 同时丢弃打开时携带的上下文
	function clearHighlight(clearCtx) {
		var marks = $('chat').querySelectorAll('mark.hl');
		for (var i = 0; i < marks.length; i++) {
			var mk = marks[i];
			var p = mk.parentNode;
			p.replaceChild(document.createTextNode(mk.textContent), mk);
			p.normalize();
		}
		$('hl-bar').classList.add('hidden');
		state.hl = null;
		if (clearCtx !== false) state.hlCtx = null;
	}

	function highlightConversation(ctx) {
		clearHighlight(false);
		var chat = $('chat');
		if (!chat.childNodes.length) return;
		var terms = (ctx.terms || []).filter(function (t) { return t.alts.length; });
		var hasPos = terms.some(function (t) { return !t.negate; });
		if (!hasPos) return;
		var sc = ctx.scope || { q: true, a: true };
		var hit = function (t, text) {
			if (!text) return false;
			if (ctx.cs) {
				for (var i = 0; i < t.alts.length; i++) if (text.indexOf(t.alts[i]) >= 0) return true;
				return false;
			}
			var low = text.toLowerCase();
			for (var j = 0; j < t.alts.length; j++) if (low.indexOf(t.alts[j].toLowerCase()) >= 0) return true;
			return false;
		};
		// 836: 与服务端 msgQualifies 同规则 —— 命中轮=同轮内普通正词凑在同一边
		// (问:/答: 固定边)且该轮不含排除词；只高亮命中轮里参与命中的边上的词。
		(state.msgs || []).forEach(function (m, i) {
			var ok = true, hasPlain = false, plainQ = true, plainA = true;
			var hiQ = [], hiA = [];
			terms.forEach(function (t) {
				if (t.negate) return;
				if (t.field === 'q') {
					if (!hit(t, m.q)) ok = false;
					else hiQ.push(t);
				} else if (t.field === 'a') {
					if (!hit(t, m.a)) ok = false;
					else hiA.push(t);
				} else {
					hasPlain = true;
					if (sc.q && !hit(t, m.q)) plainQ = false;
					if (sc.a && !hit(t, m.a)) plainA = false;
				}
			});
			var sideQ = !!(sc.q && plainQ), sideA = !!(sc.a && plainA);
			if (ok && hasPlain && !sideQ && !sideA) ok = false; // 普通词没凑在同一边
			if (ok) {
				terms.forEach(function (t) {
					if (!t.negate) return;
					if (t.field === 'q') { if (hit(t, m.q)) ok = false; }
					else if (t.field === 'a') { if (hit(t, m.a)) ok = false; }
					else if ((sc.q && hit(t, m.q)) || (sc.a && hit(t, m.a))) ok = false;
				});
			}
			if (!ok) return;
			if (sideQ) terms.forEach(function (t) { if (!t.negate && !t.field) hiQ.push(t); });
			if (sideA) terms.forEach(function (t) { if (!t.negate && !t.field) hiA.push(t); });
			if (hiQ.length) hlWithin(chat.querySelector('.msg[data-hi-turn="' + i + '"][data-hi-side="q"]'), hiQ, ctx.cs);
			if (hiA.length) hlWithin(chat.querySelector('.msg[data-hi-turn="' + i + '"][data-hi-side="a"]'), hiA, ctx.cs);
		});
		var marks = chat.querySelectorAll('mark.hl');
		if (!marks.length) {
			toast('该会话内没有可高亮的内容');
			return;
		}
		state.hl = { marks: marks, pos: -1 };
		$('hl-bar').classList.remove('hidden');
		hlGo(1); // 定位到第一处命中轮
	}

	// hlWithin 只在指定子树内包裹高亮(836: 每轮一个根节点)
	function hlWithin(root, matchers, cs) {
		if (!root) return;
		var walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
			acceptNode: function (n) {
				var nn = n.parentNode ? n.parentNode.nodeName : '';
				if (nn === 'MARK' || nn === 'SCRIPT' || nn === 'STYLE') return NodeFilter.FILTER_REJECT;
				return n.nodeValue ? NodeFilter.FILTER_ACCEPT : NodeFilter.FILTER_REJECT;
			},
		});
		var nodes = [];
		while (walker.nextNode()) nodes.push(walker.currentNode);
		nodes.forEach(function (n) { hlWrapNode(n, matchers, cs); });
	}

	function hlWrapNode(node, matchers, cs) {
		var text = node.nodeValue;
		if (!text) return;
		var ranges = []; // [start, end]，所有条件所有候选的命中合并
		var low = cs ? null : text.toLowerCase();
		matchers.forEach(function (mt) {
			mt.alts.forEach(function (kw) {
				if (cs) {
					var a = text.indexOf(kw);
					while (a >= 0 && ranges.length < 500) {
						ranges.push([a, a + kw.length]);
						a = text.indexOf(kw, a + kw.length);
					}
				} else {
					var kl = kw.toLowerCase();
					var b = low.indexOf(kl);
					while (b >= 0 && ranges.length < 500) {
						ranges.push([b, b + kw.length]);
						b = low.indexOf(kl, b + kl.length);
					}
				}
			});
		});
		if (!ranges.length) return;
		ranges.sort(function (x, y) { return x[0] - y[0] || x[1] - y[1]; });
		var frag = document.createDocumentFragment();
		var pos = 0;
		ranges.forEach(function (r) {
			if (r[0] < pos) return; // 重叠保护
			if (r[0] > pos) frag.appendChild(document.createTextNode(text.slice(pos, r[0])));
			var mk = document.createElement('mark');
			mk.className = 'hl';
			mk.textContent = text.slice(r[0], r[1]);
			frag.appendChild(mk);
			pos = r[1];
		});
		if (pos < text.length) frag.appendChild(document.createTextNode(text.slice(pos)));
		node.parentNode.replaceChild(frag, node);
	}

	function hlGo(step) {
		var hl = state.hl;
		if (!hl || !hl.marks.length) return;
		hl.pos += step;
		if (hl.pos >= hl.marks.length) hl.pos = 0; // 循环跳转
		if (hl.pos < 0) hl.pos = hl.marks.length - 1;
		for (var i = 0; i < hl.marks.length; i++) hl.marks[i].classList.remove('cur', 'flash');
		var cur = hl.marks[hl.pos];
		cur.classList.add('cur', 'flash');
		$('hl-count').textContent = (hl.pos + 1) + '/' + hl.marks.length;
		cur.scrollIntoView({ behavior: 'smooth', block: 'center' });
	}

	function el(tag, cls, text) {
		var e = document.createElement(tag);
		if (cls) e.className = cls;
		if (text != null) e.textContent = text;
		return e;
	}

	async function api(path, opts) {
		opts = opts || {};
		var headers = Object.assign({ 'Content-Type': 'application/json' }, opts.headers || {});
		if (state.token) headers['Authorization'] = 'Bearer ' + state.token;
		var res = await fetch(path, Object.assign({}, opts, { headers: headers }));
		if (res.status === 401 && path !== '/hybapi/login') {
			state.token = '';
			localStorage.removeItem(LS_TOKEN);
			showLogin();
			throw new Error('未登录或会话已过期');
		}
		return res;
	}

	async function apiJSON(path, opts) {
		var r = await api(path, opts);
		var d = null;
		try { d = await r.json(); } catch (e) { /* ignore */ }
		if (!r.ok) throw new Error((d && d.error && d.error.message) || 'HTTP ' + r.status);
		return d;
	}

	/* ---------------- 视图切换 ---------------- */

	function showLogin() {
		$('app-view').classList.add('hidden');
		$('login-view').classList.remove('hidden');
		$('login-error').textContent = '';
		setTimeout(function () { $('login-username').focus(); }, 50);
	}

	function showApp() {
		$('login-view').classList.add('hidden');
		$('app-view').classList.remove('hidden');
	}

	/* ---------------- 登录 ---------------- */

	// 413: 密码 明文/密文 切换
	$('pw-eye').addEventListener('click', function (e) {
		e.preventDefault();
		var pw = $('login-password');
		var show = pw.type === 'password';
		pw.type = show ? 'text' : 'password';
		$('pw-eye-on').classList.toggle('hidden', show);
		$('pw-eye-off').classList.toggle('hidden', !show);
	});

	$('login-form').addEventListener('submit', async function (e) {
		e.preventDefault();
		var btn = $('login-btn');
		btn.disabled = true;
		$('login-error').textContent = '';
		try {
			var res = await api('/hybapi/login', {
				method: 'POST',
				body: JSON.stringify({
					username: $('login-username').value.trim(),
					password: $('login-password').value,
				}),
			});
			var d = await res.json();
			if (!res.ok) throw new Error((d && d.error && d.error.message) || '登录失败');
			state.token = d.token;
			localStorage.setItem(LS_TOKEN, d.token);
			state.user = d.user;
			showApp();
			await initApp();
		} catch (err) {
			$('login-error').textContent = err.message || '登录失败';
		} finally {
			btn.disabled = false;
		}
	});

	// 33: 退出登录前弹确认框，确认后才登出(93/94 起该确认框通用化,供删除密钥等复用)
	var pendingConfirm = null;
	function askConfirm(title, okLabel, cb) {
		pendingConfirm = cb;
		$('cf-title').textContent = title;
		$('cf-ok').textContent = okLabel || '确认';
		$('confirm-pop').classList.remove('hidden');
	}
	$('btn-logout').addEventListener('click', function () {
		askConfirm('确认退出登录？', '退出登录', doLogout);
	});
	function doLogout() {
		(async function () {
			try { await api('/hybapi/logout', { method: 'POST' }); } catch (e) { /* ignore */ }
			state.token = '';
			localStorage.removeItem(LS_TOKEN);
			location.reload();
		})();
	}
	$('cf-cancel').addEventListener('click', function () {
		$('confirm-pop').classList.add('hidden');
		pendingConfirm = null;
	});
	$('confirm-pop').addEventListener('click', function (e) {
		if (e.target === this) { $(this.id).classList.add('hidden'); pendingConfirm = null; }
	});
	$('cf-ok').addEventListener('click', function () {
		$('confirm-pop').classList.add('hidden');
		var cb = pendingConfirm;
		pendingConfirm = null;
		if (cb) cb();
	});

	/* ---------------- 93/94/95: 个人中心(用户菜单 + 修改密码/API密钥/钱包) ---------------- */

	function closeUserMenu() {
		$('u-menu').classList.add('hidden');
		$('u-center').setAttribute('aria-expanded', 'false');
	}
	$('u-center').addEventListener('click', function () {
		var m = $('u-menu');
		var open = m.classList.contains('hidden');
		m.classList.toggle('hidden', !open);
		$('u-center').setAttribute('aria-expanded', open ? 'true' : 'false');
	});
	document.addEventListener('click', function (e) {
		if (!e.target.closest('#u-center') && !e.target.closest('#u-menu')) closeUserMenu();
	});

	// 弹框通用:点遮罩 / Esc / data-close 关闭
	function openDlg(id) {
		closeUserMenu();
		$(id).classList.remove('hidden');
	}
	function closeDlg(id) { $(id).classList.add('hidden'); }
	Array.prototype.forEach.call(document.querySelectorAll('.dlg-pop'), function (d) {
		d.addEventListener('click', function (e) { if (e.target === d) d.classList.add('hidden'); });
	});
	Array.prototype.forEach.call(document.querySelectorAll('[data-close]'), function (b) {
		b.addEventListener('click', function () { closeDlg(b.getAttribute('data-close')); });
	});
	document.addEventListener('keydown', function (e) {
		if (e.key !== 'Escape') return;
		Array.prototype.forEach.call(document.querySelectorAll('.dlg-pop'), function (d) {
			if (!d.classList.contains('hidden')) d.classList.add('hidden');
		});
		closeUserMenu();
	});

	function copyText(t) {
		if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(t);
		var ta = document.createElement('textarea');
		ta.value = t;
		ta.style.position = 'fixed';
		ta.style.opacity = '0';
		document.body.appendChild(ta);
		ta.select();
		try { document.execCommand('copy'); } catch (e) { /* ignore */ }
		ta.remove();
		return Promise.resolve();
	}

	// 93: 修改密码
	function pwErr(msg) { $('pw-err').textContent = msg || ''; }
	['pw-old', 'pw-new', 'pw-new2'].forEach(function (id) {
		$(id).addEventListener('input', function () { pwErr(''); });
		$(id).addEventListener('keydown', function (e) { if (e.key === 'Enter') $('pw-ok').click(); });
	});
	$('mi-pass').addEventListener('click', function () {
		['pw-old', 'pw-new', 'pw-new2'].forEach(function (id) { $(id).value = ''; });
		pwErr('');
		openDlg('dlg-pass');
		setTimeout(function () { $('pw-old').focus(); }, 50);
	});
	$('pw-ok').addEventListener('click', async function () {
		var oldPw = $('pw-old').value, n1 = $('pw-new').value, n2 = $('pw-new2').value;
		if (!oldPw || !n1 || !n2) { pwErr('请把三项都填上'); return; }
		if (n1.length < 8 || n1.length > 20) { pwErr('新密码长度需为 8~20 位'); return; }
		if (n1 !== n2) { pwErr('两次输入的新密码不一致'); return; }
		var btn = $('pw-ok');
		btn.disabled = true;
		try {
			await apiJSON('/hybapi/password', { method: 'POST', body: JSON.stringify({ original: oldPw, password: n1 }) });
			closeDlg('dlg-pass');
			toast('密码已修改');
		} catch (e) {
			pwErr(e.message || '修改失败');
		} finally {
			btn.disabled = false;
		}
	});

	// 94: API 密钥
	function keyErr(msg) { $('key-err').textContent = msg || ''; }
	async function loadKeys() {
		var d = await apiJSON('/hybapi/keys');
		var list = $('key-list');
		list.textContent = '';
		(d.data || []).forEach(function (k) {
			var row = el('div', 'key-row');
			var info = el('div', 'key-info');
			var name = el('div', 'key-name');
			name.appendChild(document.createTextNode(k.name));
			if (k.is_default) {
				var tag = el('span', 'key-tag', '默认·不可删');
				name.appendChild(tag);
			}
			info.appendChild(name);
			info.appendChild(el('div', 'key-mono', k.masked));
			info.appendChild(el('div', 'key-date', '创建于 ' + fmtTime(k.created_at * 1000)));
			// 94b: 摘要行 额度/模型/IP
			var quotaTxt = k.unlimited_quota
				? '额度不限'
				: '额度 已用 ' + fmtMoney(k.used_quota, 500000) + ' / 剩余 ' + fmtMoney(k.remain_quota, 500000);
			var modelTxt = k.model_limits_enabled ? '模型仅' + k.model_limits.length + '个' : '模型不限';
			var ipTxt = (k.allow_ips || []).length ? 'IP白名单' + k.allow_ips.length + '条' : 'IP不限';
			info.appendChild(el('div', 'key-sub', quotaTxt + ' · ' + modelTxt + ' · ' + ipTxt));
			row.appendChild(info);
			var ops = el('div', 'key-ops');
			var cp = el('button', 'key-btn', '复制');
			cp.type = 'button';
			cp.addEventListener('click', async function () {
				cp.disabled = true;
				try {
					var full = await apiJSON('/hybapi/keys/' + k.id);
					await copyText(full.key);
					toast('密钥已复制');
				} catch (e) {
					toast(e.message || '复制失败');
				} finally {
					cp.disabled = false;
				}
			});
			ops.appendChild(cp);
			if (!k.is_default) {
				var setBtn = el('button', 'key-btn', '设置');
				setBtn.type = 'button';
				setBtn.addEventListener('click', function () { openKeyEdit(k); });
				ops.appendChild(setBtn);
				var del = el('button', 'key-btn key-del', '删除');
				del.type = 'button';
				del.addEventListener('click', function () {
					askConfirm('删除密钥「' + k.name + '」？\n用它的程序将立即无法调用。', '删除', async function () {
						try {
							await apiJSON('/hybapi/keys/' + k.id, { method: 'DELETE' });
							toast('已删除');
							 await loadKeys();
						} catch (e) {
							keyErr(e.message || '删除失败');
						}
					});
				});
				ops.appendChild(del);
			}
			row.appendChild(ops);
			list.appendChild(row);
		});
		if (!(d.data || []).length) list.appendChild(el('div', 'key-date', '还没有密钥'));
	}
	$('mi-keys').addEventListener('click', async function () {
		keyErr('');
		$('key-name').value = '';
		openDlg('dlg-keys');
		try { await loadKeys(); } catch (e) { keyErr(e.message || '加载失败'); }
	});
	$('key-add').addEventListener('click', async function () {
		var btn = $('key-add');
		btn.disabled = true;
		keyErr('');
		try {
			var d = await apiJSON('/hybapi/keys', { method: 'POST', body: JSON.stringify({ name: $('key-name').value }) });
			$('key-name').value = '';
			await loadKeys();
			copyText(d.key).then(function () { toast('已创建并复制新密钥'); }, function () { toast('已创建新密钥'); });
		} catch (e) {
			keyErr(e.message || '创建失败');
		} finally {
			btn.disabled = false;
		}
	});
	$('key-name').addEventListener('keydown', function (e) { if (e.key === 'Enter') $('key-add').click(); });

	// 94b: 密钥设置(额度$/勾选模型/IP 白名单);default 无「设置」按钮,后端也拒绝。
	var keId = 0;
	function keErr(msg) { $('ke-err').textContent = msg || ''; }
	function keRadio(name) {
		var els = document.querySelectorAll('input[name="' + name + '"]');
		for (var i = 0; i < els.length; i++) if (els[i].checked) return els[i].value;
		return '';
	}
	function keSyncVis() {
		document.querySelector('.ke-quota-input').classList.toggle('hidden', keRadio('ke-quota') !== 'limit');
		$('ke-models').classList.toggle('hidden', keRadio('ke-model') !== 'some');
	}
	Array.prototype.forEach.call(document.querySelectorAll('input[name="ke-quota"],input[name="ke-model"]'), function (r) {
		r.addEventListener('change', keSyncVis);
	});
	function openKeyEdit(k) {
		keId = k.id;
		keErr('');
		$('ke-title').textContent = '密钥设置「' + k.name + '」';
		(function (v) {
			var els = document.querySelectorAll('input[name="ke-quota"]');
			for (var i = 0; i < els.length; i++) els[i].checked = (els[i].value === v);
		})(k.unlimited_quota ? 'unlimited' : 'limit');
		(function (v) {
			var els = document.querySelectorAll('input[name="ke-model"]');
			for (var i = 0; i < els.length; i++) els[i].checked = (els[i].value === v);
		})(k.model_limits_enabled ? 'some' : 'all');
		$('ke-usd').value = k.unlimited_quota ? '' : String(Math.round(k.remain_quota / 500000 * 10000) / 10000);
		$('ke-ips').value = (k.allow_ips || []).join('\n');
		// 勾选列表 = 聊天可用模型;已限制但不在列表里的模型名补在后面,避免保存时被悄悄丢掉。
		var box = $('ke-models');
		box.textContent = '';
		var cur = {};
		(k.model_limits || []).forEach(function (m) { cur[m] = true; });
		var names = state.models.map(function (m) { return m.id; });
		(k.model_limits || []).forEach(function (m) { if (names.indexOf(m) < 0) names.push(m); });
		names.forEach(function (m) {
			var lab = el('label', null);
			var cb = document.createElement('input');
			cb.type = 'checkbox';
			cb.value = m;
			cb.checked = !!cur[m];
			lab.appendChild(cb);
			lab.appendChild(document.createTextNode(' ' + (modelLabel(m) || m)));
			box.appendChild(lab);
		});
		keSyncVis();
		openDlg('dlg-keyedit');
	}
	$('ke-ok').addEventListener('click', async function () {
		if (!keId) return;
		keErr('');
		var unlimited = keRadio('ke-quota') === 'unlimited';
		var body = { unlimited_quota: unlimited, model_limits_enabled: keRadio('ke-model') === 'some', allow_ips: [] };
		if (!unlimited) {
			var usd = parseFloat($('ke-usd').value);
			if (isNaN(usd) || usd < 0) { keErr('请填写不小于 0 的额度金额（美元）'); return; }
			body.remain_usd = usd;
		}
		if (body.model_limits_enabled) {
			body.model_limits = Array.prototype.map.call($('ke-models').querySelectorAll('input:checked'), function (cb) { return cb.value; });
			if (!body.model_limits.length) { keErr('开启了模型限制但没有勾选任何模型'); return; }
		} else {
			body.model_limits = [];
		}
		body.allow_ips = $('ke-ips').value.split(/[\n,]+/).map(function (s) { return s.trim(); }).filter(Boolean);
		var btn = $('ke-ok');
		btn.disabled = true;
		try {
			await apiJSON('/hybapi/keys/' + keId, { method: 'PUT', body: JSON.stringify(body) });
			closeDlg('dlg-keyedit');
			toast('已保存');
			await loadKeys();
		} catch (e) {
			keErr(e.message || '保存失败');
		} finally {
			btn.disabled = false;
		}
	});

	// 95: 钱包(只读):941 每模型已用 tokens/金额;942 剩余金额单独用于该模型的估算
	function fmtMoney(quota, perUnit) {
		var v = quota / (perUnit || 500000);
		if (v >= 1000) return '$' + v.toFixed(0);
		if (v >= 100) return '$' + v.toFixed(1);
		if (v >= 1) return '$' + v.toFixed(2);
		return '$' + v.toFixed(4);
	}
	function fmtTokens(n) {
		if (n >= 1e8) return (n / 1e8).toFixed(1) + '亿';
		if (n >= 1e4) return (n / 1e4).toFixed(1) + '万';
		return String(Math.floor(n));
	}
	function estText(v) {
		if (!v || v <= 0) return '—';
		if (v < 1) return '不足 1';
		return '≈ ' + fmtTokens(v);
	}
	$('mi-wallet').addEventListener('click', async function () {
		openDlg('dlg-wallet');
		$('wal-remain').textContent = '…';
		$('wal-used').textContent = '…';
		$('wal-list').textContent = '';
		$('wal-note').textContent = '';
		var ids = state.models.map(function (m) { return m.id; }).join(',');
		try {
			var d = await apiJSON('/hybapi/wallet' + (ids ? '?models=' + encodeURIComponent(ids) : ''));
			$('wal-remain').textContent = fmtMoney(d.remain_quota, d.per_unit);
			$('wal-used').textContent = fmtMoney(d.used_quota, d.per_unit);
			var list = $('wal-list');
			var head = el('div', 'wal-head');
			[{ t: '模型' }, { t: '已用 tokens' }, { t: '已花费' }, { t: '还可 tokens', sub: '若余额单单只用于对应模型' }, { t: '还可次数', sub: '若余额单单只用于对应模型' }].forEach(function (h) {
				var d = el('div', null, h.t);
				if (h.sub) d.appendChild(el('span', 'wal-h2', '(' + h.sub + ')'));
				head.appendChild(d);
			});
			list.appendChild(head);
			(d.models || []).forEach(function (m) {
				var row = el('div', 'wal-row');
				var name = el('div', 'wal-model', m.a_model || m.model);
				name.title = m.model;
				row.appendChild(name);
				var usedTok = m.used
					? '入 ' + fmtTokens(m.prompt_tokens) + ' / 出 ' + fmtTokens(m.completion_tokens)
					: '—';
				row.appendChild(el('div', 'wal-cell', usedTok));
				row.appendChild(el('div', 'wal-cell', m.used ? fmtMoney(m.quota, d.per_unit) : '—'));
				row.appendChild(el('div', 'wal-cell', m.by_call ? '按次计费' : estText(m.est_tokens)));
				row.appendChild(el('div', 'wal-cell', estText(m.est_calls)));
				list.appendChild(row);
			});
			if (!(d.models || []).length) list.appendChild(el('div', 'wal-note', '暂无消费记录'));
			$('wal-note').textContent = '「还可」为估算值：有消费记录的模型按其历史平均消耗估算；未使用的模型按每次 输入1K+输出0.5K tokens 的典型对话结合牌价估算；按次计费模型只估次数。剩余金额若单独用于该模型，即为该行的估算结果。';
		} catch (e) {
			$('wal-note').textContent = e.message || '加载失败';
		}
	});


	/* ---------------- 初始化 ---------------- */

	var inited = false;

	async function initApp() {
		fillUser();
		if (!inited) {
			wireEvents();
			inited = true;
		}
		// 32: 恢复上次的侧边栏收起状态(桌面)
		if (localStorage.getItem('hyb_side') === '0') $('app-view').classList.add('side-hidden');
		$('sysp').value = state.sysp;
		await loadModels();
		await loadSenList();
		newChat(false);
	}

	function fillUser() {
		var u = state.user || {};
		var name = u.display_name || u.username || '?';
		$('u-name').textContent = name;
		$('u-role').textContent = u.role === 'admin' ? '管理员' : '用户';
		$('u-avatar').textContent = name.charAt(0).toUpperCase();
	}


	async function loadModels() {
		try {
			var d = await apiJSON('/hybapi/models');
			state.models = d.data || [];
		} catch (e) {
			state.models = [];
			toast('获取模型列表失败: ' + e.message);
		}
		var sel = $('model-sel');
		sel.innerHTML = '';
		state.models.forEach(function (m) {
			var o = document.createElement('option');
			o.value = m.id;
			o.textContent = m.name || m.id;
			sel.appendChild(o);
		});
		if (state.models.length) {
			var has = state.models.some(function (m) { return m.id === state.model; });
			if (!has) state.model = state.models[0].id;
			sel.value = state.model;
		} else {
			var o = document.createElement('option');
			o.textContent = '无可用模型';
			sel.appendChild(o);
		}
	}

	/* ---------------- 会话列表 ---------------- */

	// 互斥时间区间：每个会话按 updated_at 只归入一个分组
	// （旧实现的判断是累积的"≥某时间"，导致同一会话出现在多个分组下）。
	var DAY = 86400;

	function startOfToday() {
		var d = new Date();
		d.setHours(0, 0, 0, 0);
		return Math.floor(d.getTime() / 1000);
	}

	function senGroups() {
		var t0 = startOfToday();
		return [
			{ name: '今天', from: t0, cls: 'g-today' },
			{ name: '昨天', from: t0 - DAY, to: t0, cls: 'g-today' },
			{ name: '近 7 天', from: t0 - 7 * DAY, to: t0 - DAY, cls: 'g-week' },
			{ name: '近 30 天', from: t0 - 30 * DAY, to: t0 - 7 * DAY, cls: 'g-week' },
			{ name: '更早', to: t0 - 30 * DAY, cls: 'g-old' },
		];
	}

	async function loadSenList() {
		try {
			// 31/71/835: 搜索由后端执行；范围 chips + 大小写/日期 组合成参数
			// (服务端解析 空格AND / 引号短语 / -排除 / A|B / 问:答: 前缀)
			var qs = [];
			var kw = $('search').value.trim();
			if (kw) {
				qs.push('q=' + encodeURIComponent(kw));
				var sc = [];
				if (state.filters.q) sc.push('q');
				if (state.filters.a) sc.push('a');
				if (sc.length) qs.push('sc=' + sc.join(','));
				if (state.filters.cs) qs.push('cs=1');
			}
			if (state.filters.date) {
				var dfrom = dateInputISO($('date-from').value);
				var dto = dateInputISO($('date-to').value);
				if (dfrom) qs.push('dfrom=' + dfrom);
				if (dto) qs.push('dto=' + dto);
			}
			if (state.includeKey) qs.push('senKey=1');
			var d = await apiJSON('/hybapi/sen' + (qs.length ? '?' + qs.join('&') : ''));
			state.senList = d.data || [];
		} catch (e) {
			state.senList = [];
			toast('加载会话列表失败: ' + e.message);
		}
		renderSenList();
	}

	function renderSenList() {
		var box = $('sen-list');
		box.innerHTML = '';
		var list = state.senList;
		if (!list.length) {
			box.appendChild(el('div', 'sen-empty', $('search').value.trim() ? '没有匹配的会话' : '暂无会话'));
			return;
		}
		senGroups().forEach(function (g) {
			var items = list.filter(function (s) {
				var t = s.updated_at || s.created_at || 0;
				return t >= (g.from || 0) && (g.to == null || t < g.to);
			});
			if (!items.length) return;
			box.appendChild(el('div', 'sen-group ' + g.cls, g.name));
			items.forEach(function (s) {
				box.appendChild(senItem(s));
			});
		});
	}

	function senItem(s) {
		var item = el('div', 'sen-item' + (s.id === state.senId ? ' cur' : ''));
		item.dataset.id = s.id;
		var main = el('div', 'sen-main');
		// 标题上方: (斜体)日期跨度 + 问答轮数, 如 26/08/01~26/08/15 共11轮问答
		var first = tsToDate(s.first_at || s.created_at);
		var last = tsToDate(s.last_at || s.updated_at);
		var span = first === last ? first : first + '~' + last;
		main.appendChild(el('div', 'sen-sub', span + ' 共' + (s.msg_count || 0) + '轮问答'));
		main.appendChild(el('div', 'sen-name', s.title || '新会话'));
		item.appendChild(main);
		if (s.isKey === 1) item.appendChild(el('span', 'sen-tag', 'API'));
		var act = el('span', 'sen-act');
		var re = el('button');
		re.type = 'button';
		re.setAttribute('data-tooltip', '重命名');
		re.innerHTML = '<svg viewBox="0 0 24 24"><path d="M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>';
		re.addEventListener('click', function (e) {
			e.stopPropagation();
			var name = prompt('重命名会话', s.title || '');
			if (name == null) return;
			name = name.trim();
			if (!name) return;
			apiJSON('/hybapi/sen/' + s.id, { method: 'PATCH', body: JSON.stringify({ title: name }) })
				.then(function () { s.title = name; renderSenList(); if (s.id === state.senId) $('top-title').textContent = name; })
				.catch(function (err) { toast(err.message); });
		});
		var del = el('button');
		del.type = 'button';
		del.setAttribute('data-tooltip', '删除');
		del.innerHTML = '<svg viewBox="0 0 24 24"><path d="M3 6h18M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2m3 0v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6M10 11v6M14 11v6" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>';
		del.addEventListener('click', function (e) {
			e.stopPropagation();
			if (!confirm('删除会话「' + (s.title || '') + '」？该操作不可恢复。')) return;
			apiJSON('/hybapi/sen/' + s.id, { method: 'DELETE' })
				.then(function () {
					if (s.id === state.senId) newChat();
					else loadSenList();
				})
				.catch(function (err) { toast(err.message); });
		});
		act.appendChild(re);
		act.appendChild(del);
		item.appendChild(act);
		item.addEventListener('click', function () {
			// 72: 搜索态下点击结果，携带解析后的条件用于打开后高亮；836: 带上范围
			// chips，只高亮「命中轮」(同轮同侧满足全部条件的那几轮)
			var kw = $('search').value.trim();
			state.hlCtx = kw ? {
				terms: parseTerms(kw),
				cs: state.filters.cs,
				scope: { q: !!state.filters.q, a: !!state.filters.a },
			} : null;
			openSen(s.id);
		});
		return item;
	}

	/* ---------------- 会话内容 ---------------- */

	async function openSen(id) {
		if (state.streaming) { toast('正在生成回复，请稍候'); return; }
		try {
			var d = await apiJSON('/hybapi/sen/' + id + '/messages');
			state.msgs = d.data || [];
		} catch (e) {
			toast(e.message);
			return;
		}
		state.senId = id;
		var sen = state.senList.find(function (s) { return s.id === id; });
		$('top-title').textContent = (sen && sen.title) || '对话';
		var chat = $('chat');
		chat.innerHTML = '';
		$('hl-bar').classList.add('hidden');
		state.hl = null;
		state.msgs.forEach(function (m, i) {
			var u = userMsgNode(m.q, m.q_at, m.q_fil_meta);
			u.dataset.hiTurn = i; // 836: 高亮按轮定位(问题/AI 各为一个 .msg)
			u.dataset.hiSide = 'q';
			chat.appendChild(u);
			var b = botMsgNode(m.a, m.a_at, m.a_model, m.a_fil_meta, m.dur_ms, m.tokens_in, m.tokens_out, m.ttft, m.tps);
			b.dataset.hiTurn = i;
			b.dataset.hiSide = 'a';
			chat.appendChild(b);
		});
		if (!state.msgs.length) chat.appendChild(welcomeNode());
		renderSenList();
		closeDrawer();
		scrollBottom(true);
		// 72: 从搜索结果进入且带关键词 → 高亮并定位到第一处
		if (state.hlCtx && state.msgs.length) {
			var ctx = state.hlCtx;
			state.hlCtx = null;
			highlightConversation(ctx);
		}
	}

	// 71/835: 应用筛选 chips 状态(问题/答案/日期/Aa)与日期行显隐
	function applyChips() {
		['q', 'a', 'date', 'cs'].forEach(function (k) {
			var b = document.querySelector('.fchip[data-scope="' + k + '"]');
			if (b) b.classList.toggle('on', !!state.filters[k]);
		});
		$('sb-dates').classList.toggle('hidden', !state.filters.date);
	}

	function newChat(focus) {
		state.senId = 0;
		state.msgs = [];
		$('top-title').textContent = '新会话';
		var chat = $('chat');
		chat.innerHTML = '';
		chat.appendChild(welcomeNode());
		clearHighlight();
		renderSenList();
		closeDrawer();
		if (focus !== false) $('input').focus();
		scrollBottom(true);
	}

	function welcomeNode() {
		var w = el('div', 'welcome');
		w.appendChild(el('div', 'w-title', '今天我能为您做些什么？'));
		w.appendChild(el('div', 'w-sub', state.model ? '当前模型: ' + modelLabel(state.model) : '请在右上角选择模型'));
		var sugs = el('div', 'w-sugs');
		['介绍一下你自己', '帮我写一首关于秋天的短诗', '用通俗的语言解释什么是大语言模型', '把「你好，世界」翻译成英文和日文'].forEach(function (s) {
			var b = el('button', 'w-sug', s);
			b.type = 'button';
			b.addEventListener('click', function () {
				$('input').value = s;
				autoGrow();
				updateSendBtn();
				$('input').focus();
			});
			sugs.appendChild(b);
		});
		w.appendChild(sugs);
		return w;
	}

	function userMsgNode(text, at, fileMeta) {
		// 用户消息不显示头像（用户要求），仅右对齐气泡
		var row = el('div', 'msg msg-user');
		var body = el('div', 'm-body');
		if (at) body.appendChild(el('div', 'm-head', fmtTime(at)));
		body.appendChild(el('div', 'm-bubble', text || '(空)'));
		if (fileMeta && fileMeta.length) body.appendChild(filesNode(fileMeta, true));
		row.appendChild(body);
		return row;
	}

	// headNode 组装 AI 消息头：渠道/模型 · 时间 · dur · tokens。
	// 有思考内容时，行末再挂一个纯三角形折叠按钮（见 attachThinkUI）。
	function headNode(model, at) {
		var head = el('div', 'm-head');
		if (model) head.appendChild(el('span', 'm-model', model));
		if (at) head.appendChild(el('span', null, fmtTime(at)));
		return head;
	}

	// 51: 元信息顺序固定 渠道/模型 · 时间 · dur · ttft · tps · tokens · ▶
	function headToggle(head) {
		for (var i = head.childNodes.length - 1; i >= 0; i--) {
			var n = head.childNodes[i];
			if (n.nodeType === 1 && n.classList && n.classList.contains('m-th-toggle')) return n;
		}
		return null;
	}

	// 64: 响应完成后才把时间插到头部(发送时只显示 渠道/模型)
	function insertHeadTime(head, at) {
		if (!at) return;
		head.insertBefore(el('span', null, fmtTime(at)), headToggle(head));
	}

	function addHeadMeta(head, durMS, tokIn, tokOut, ttft, tps) {
		var spans = [];
		if (durMS != null) spans.push(el('span', 'm-dur', 'dur:' + (durMS / 1000).toFixed(1) + 's'));
		if (ttft > 0) spans.push(el('span', 'm-ttft', 'ttft:' + (ttft / 1000).toFixed(1) + 's'));
		if (tps > 0) spans.push(el('span', 'm-tps', 'tps:' + tps + '/s'));
		if (tokIn > 0 || tokOut > 0) spans.push(el('span', 'm-tok', 'tokens=in:' + tokIn + '/out:' + tokOut));
		if (!spans.length) return;
		// 三角形若已创建(流式期间收到 reasoning 时)，插到它前面，
		// 保证最终顺序: 渠道/模型 时间 dur ttft tps tokens ▶
		var anchor = headToggle(head);
		spans.forEach(function (s) { head.insertBefore(s, anchor); });
	}

	// attachThinkUI 在消息头末尾放一个三角形开关，点击展开/收起思考内容
	// （思考内容插在 head 与正文之间）。无思考则不调用、不显示三角形。
	function attachThinkUI(head, body, beforeEl) {
		var btn = el('button', 'm-th-toggle');
		btn.type = 'button';
		btn.setAttribute('data-tooltip', '思考过程');
		var wrap = el('div', 'm-th hidden');
		btn.addEventListener('click', function () {
			wrap.classList.toggle('hidden');
			btn.classList.toggle('open');
		});
		head.appendChild(btn);
		body.insertBefore(wrap, beforeEl || null);
		return { btn: btn, wrap: wrap };
	}

	function botMsgNode(text, at, model, fileMeta, durMS, tokIn, tokOut, ttft, tps) {
		var row = el('div', 'msg');
		var av = el('div', 'm-avatar a-bot');
		av.textContent = modelLetter(model);
		row.appendChild(av);
		var body = el('div', 'm-body');
		var head = headNode(model, at);
		addHeadMeta(head, durMS, tokIn, tokOut, ttft, tps);
		if (head.childNodes.length) body.appendChild(head);
		var md = el('div', 'm-md');
		var s = text || '';
		if (/^\[报错Error\]/.test(s)) {
			md.classList.add('m-err');
			md.textContent = s;
		} else if (s) {
			md.innerHTML = renderMarkdown(s);
		} else {
			md.classList.add('m-err');
			md.textContent = '(无响应)';
		}
		body.appendChild(md);
		if (fileMeta && fileMeta.length) body.appendChild(filesNode(fileMeta, false));
		row.appendChild(body);
		return row;
	}

	function filesNode(meta, isUser) {
		var box = el('div', 'm-files');
		meta.forEach(function (f) {
			var chip = el('div', 'file-chip');
			var isImg = /^image\//.test(f.type || '') || /\.(png|jpe?g|gif|webp|bmp)$/i.test(f.name || '');
			if (isImg) {
				var img = document.createElement('img');
				img.className = 'f-thumb';
				img.src = f.url;
				img.loading = 'lazy';
				chip.appendChild(img);
				chip.classList.add('clickable');
				chip.addEventListener('click', function () { openLightbox(f.url, f.name); });
			} else {
				chip.appendChild(el('span', 'f-ico', fileIcon(f.name)));
			}
			chip.appendChild(el('span', 'f-name', f.name + (f.size ? ' (' + fmtSize(f.size) + ')' : '')));
			var dl = document.createElement('a');
			dl.className = 'f-dl';
			dl.textContent = '下载';
			dl.href = f.url + '?dl=1';
			dl.setAttribute('download', f.name || 'file');
			chip.appendChild(dl);
			box.appendChild(chip);
		});
		return box;
	}

	function fileIcon(name) {
		var ext = (name || '').split('.').pop().toLowerCase();
		if (['mp3', 'wav', 'ogg', 'm4a', 'webm'].indexOf(ext) >= 0) return '🎵';
		if (['mp4', 'mov', 'avi', 'mkv'].indexOf(ext) >= 0) return '🎬';
		if (ext === 'pdf') return '📄';
		if (['zip', 'rar', '7z', 'tar', 'gz'].indexOf(ext) >= 0) return '🗜️';
		if (['doc', 'docx'].indexOf(ext) >= 0) return '📝';
		if (['xls', 'xlsx', 'csv'].indexOf(ext) >= 0) return '📊';
		return '📎';
	}

	/* ---------------- 附件（待发送） ---------------- */

	function isTextual(file) {
		if ((file.type || '').indexOf('text/') === 0) return true;
		if (/json|xml|javascript|yaml|toml|csv|markdown/i.test(file.type || '')) return true;
		return /\.(txt|md|log|csv|json|xml|yml|yaml|toml|ini|conf|js|ts|py|go|java|c|cpp|h|sh|html|css|sql)$/i.test(file.name || '');
	}

	function addFiles(fileList) {
		var files = Array.prototype.slice.call(fileList);
		files.forEach(function (f) {
			if ((f.type || '').indexOf('image/') === 0) {
				var r = new FileReader();
				r.onload = function () {
					state.files.push({ kind: 'image', name: f.name, size: f.size, type: f.type, dataUrl: r.result });
					renderFilesBar();
					updateSendBtn();
				};
				r.readAsDataURL(f);
			} else if (isTextual(f)) {
				if (f.size > 256 * 1024) {
					toast('文本附件超过 256KB，已跳过: ' + f.name);
					return;
				}
				var r2 = new FileReader();
				r2.onload = function () {
					state.files.push({ kind: 'text', name: f.name, size: f.size, type: f.type, text: String(r2.result) });
					renderFilesBar();
					updateSendBtn();
				};
				r2.readAsText(f);
			} else {
				toast('暂仅支持图片和文本类附件: ' + f.name);
			}
		});
	}

	function renderFilesBar() {
		var bar = $('files-bar');
		bar.innerHTML = '';
		if (!state.files.length) {
			bar.classList.add('hidden');
			return;
		}
		bar.classList.remove('hidden');
		state.files.forEach(function (f, idx) {
			var chip = el('div', 'file-chip');
			if (f.kind === 'image') {
				var img = document.createElement('img');
				img.className = 'f-thumb';
				img.src = f.dataUrl;
				chip.appendChild(img);
			} else {
				chip.appendChild(el('span', 'f-ico', '📄'));
			}
			chip.appendChild(el('span', 'f-name', f.name + ' (' + fmtSize(f.size) + ')'));
			var rm = el('button');
			rm.type = 'button';
			rm.setAttribute('data-tooltip', '移除');
			rm.innerHTML = '<svg viewBox="0 0 24 24"><path d="M18 6 6 18M6 6l12 12" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>';
			rm.addEventListener('click', function () {
				state.files.splice(idx, 1);
				renderFilesBar();
				updateSendBtn();
			});
			chip.appendChild(rm);
			bar.appendChild(chip);
		});
	}

	/* ---------------- 发送与流式接收 ---------------- */

	function buildMessages(inputText) {
		var msgs = [];
		if (state.sysp.trim()) msgs.push({ role: 'system', content: state.sysp.trim() });
		state.msgs.forEach(function (m) {
			if (m.q) msgs.push({ role: 'user', content: m.q });
			if (m.a) msgs.push({ role: 'assistant', content: m.a });
		});
		var images = state.files.filter(function (f) { return f.kind === 'image'; });
		var texts = state.files.filter(function (f) { return f.kind === 'text'; });
		var text = inputText;
		texts.forEach(function (f) {
			text += '\n\n[附件 ' + f.name + ']\n```\n' + f.text + '\n```';
		});
		var content;
		if (images.length) {
			var parts = [];
			var t = text.trim();
			parts.push({ type: 'text', text: t || '请描述这些图片的内容。' });
			images.forEach(function (f) {
				parts.push({ type: 'image_url', image_url: { url: f.dataUrl } });
			});
			content = parts;
		} else {
			content = text;
		}
		msgs.push({ role: 'user', content: content });
		return msgs;
	}

	async function send() {
		if (state.streaming) return;
		var input = $('input');
		var text = input.value.trim();
		if (!text && !state.files.length) return;
		if (!state.model) { toast('请先选择模型'); return; }
		hideSendError();

		var chat = $('chat');
		var welcome = chat.querySelector('.welcome');
		if (welcome) welcome.remove();

		// 立即渲染用户消息（含待发送附件缩略图；用户消息无头像）
		var pendingImgs = state.files.filter(function (f) { return f.kind === 'image'; })
			.map(function (f) { return { name: f.name, url: f.dataUrl, type: f.type }; });
		chat.appendChild(userMsgNode(text, Date.now(), pendingImgs));

		var tStart = Date.now();
		var firstChunkAt = 0; // 首个 data: chunk 到达时刻(客户端实测 ttft)
		var botBody = { think: '', content: '', renderedAt: 0, tokIn: 0, tokOut: 0 };
		var row = el('div', 'msg');
		var av = el('div', 'm-avatar a-bot');
		av.textContent = modelLetter(state.model);
		row.appendChild(av);
		var body = el('div', 'm-body');
		// 64: 发送时只显示模型名，时间等响应完成后再出现
		var head = headNode(modelLabel(state.model), null);
		body.appendChild(head);
		var thinkUI = null; // 首个 reasoning 增量到达时才创建三角形
		var md = el('div', 'm-md');
		var dots = el('div', 'typing-dots');
		dots.innerHTML = '<i></i><i></i><i></i>';
		md.appendChild(dots);
		body.appendChild(md);
		row.appendChild(body);
		chat.appendChild(row);
		scrollBottom(true);

		state.streaming = true;
		setSending(true);
		var aborter = state.aborter = new AbortController();

		var body_ = {
			model: state.model,
			stream: true,
			messages: buildMessages(text),
		};

		var finish = function (answerText, errMsg) {
			state.streaming = false;
			setSending(false);
			if (errMsg) {
				showSendError(errMsg);
				md.classList.add('m-err');
				md.textContent = '[报错Error] ' + errMsg;
				answerText = '[报错Error] ' + errMsg;
			} else {
				renderFinal();
			}
			// 51: ttft 客户端实测; tps 同后端口径(输出阶段, 突发时全程兜底)
			var durNow = Date.now() - tStart;
			var ttft = firstChunkAt ? firstChunkAt - tStart : 0;
			var gen = durNow - ttft;
			if (gen < 500) gen = durNow;
			var tps = botBody.tokOut > 0 ? Math.round(botBody.tokOut * 1000 / Math.max(gen, 1)) : 0;
			insertHeadTime(head, Date.now());
			addHeadMeta(head, durNow, botBody.tokIn, botBody.tokOut, ttft, tps);
			state.msgs.push({
				q: text,
				a: errMsg ? '[报错Error] ' + errMsg : (botBody.content || ''),
				q_at: tStart,
				a_at: Date.now(),
				a_model: state.model,
				ttft: ttft,
				tps: tps,
			});
			input.value = '';
			autoGrow();
			state.files = [];
			renderFilesBar();
			updateSendBtn();
			loadSenList();
			if (state.senId) {
				var s = state.senList.find(function (x) { return x.id === state.senId; });
				if (s) $('top-title').textContent = s.title;
			}
			input.focus();
		};

		var renderStream = function () {
			var now = Date.now();
			if (now - botBody.renderedAt < 120 && !botBody.done) return;
			botBody.renderedAt = now;
			if (botBody.think && !thinkUI) thinkUI = attachThinkUI(head, body, md);
			if (thinkUI) thinkUI.wrap.textContent = botBody.think;
			if (botBody.content) {
				md.innerHTML = renderMarkdown(botBody.content);
			} else if (!botBody.think) {
				md.innerHTML = '';
				md.appendChild(dots);
			} else {
				md.innerHTML = '';
			}
			if (state.stick) scrollBottom(false);
		};

		var renderFinal = function () {
			botBody.done = true;
			if (botBody.think && !thinkUI) thinkUI = attachThinkUI(head, body, md);
			if (thinkUI) thinkUI.wrap.textContent = botBody.think;
			md.innerHTML = '';
			var s = botBody.content;
			if (s) md.innerHTML = renderMarkdown(s);
			else { md.classList.add('m-err'); md.textContent = '(无响应)'; }
			if (state.stick) scrollBottom(false);
		};

		try {
			var headers = { 'Content-Type': 'application/json', Authorization: 'Bearer ' + state.token };
			if (state.senId) headers['X-Sen-Id'] = String(state.senId);
			var res = await fetch('/hybapi/chat/completions', {
				method: 'POST',
				headers: headers,
				body: JSON.stringify(body_),
				signal: aborter.signal,
			});
			if (res.status === 401) {
				state.token = '';
				localStorage.removeItem(LS_TOKEN);
				showLogin();
				return;
			}
			var sid = res.headers.get('X-Sen-Id');
			if (sid && parseInt(sid, 10) > 0) state.senId = parseInt(sid, 10);

			var ctype = res.headers.get('content-type') || '';
			if (!res.ok || ctype.indexOf('event-stream') < 0) {
				var d = null;
				try { d = await res.json(); } catch (e) { /* ignore */ }
				if (!res.ok || (d && d.error)) {
					finish(null, (d && d.error && d.error.message) || 'HTTP ' + res.status);
					return;
				}
				// 90: 模型/渠道不支持流式时，上游对 stream:true 也可能直接回普通
				// JSON —— 这是一份完整答案而非错误，按非流式结果渲染(无 ttft)。
				if (d.usage) {
					botBody.tokIn = d.usage.prompt_tokens | 0;
					botBody.tokOut = d.usage.completion_tokens | 0;
				}
				var ch0 = d.choices && d.choices[0];
				var am = ch0 && ch0.message;
				if (am) {
					if (am.reasoning_content) botBody.think += am.reasoning_content;
					else if (am.reasoning) botBody.think += am.reasoning;
					var ac = am.content;
					if (Array.isArray(ac)) {
						ac.forEach(function (p) { if (p && p.type === 'text' && p.text) botBody.content += p.text; });
					} else if (ac) {
						botBody.content += ac;
					}
				}
				finish();
				return;
			}

			var reader = res.body.getReader();
			var dec = new TextDecoder();
			var buf = '';
			var streamErr = null;
			while (true) {
				var chunk = await reader.read();
				if (chunk.done) break;
				buf += dec.decode(chunk.value, { stream: true });
				var lines = buf.split('\n');
				buf = lines.pop();
				for (var i = 0; i < lines.length; i++) {
					var line = lines[i].replace(/\r$/, '');
					if (!line || line.charAt(0) === ':') continue;
					if (line.indexOf('data:') !== 0) continue;
					var payload = line.slice(5).trim();
					if (payload === '[DONE]') { buf = ''; break; }
					var j = null;
					try { j = JSON.parse(payload); } catch (e) { continue; }
					if (!firstChunkAt) firstChunkAt = Date.now();
					if (j.error) {
						streamErr = (j.error.message || JSON.stringify(j.error));
						continue;
					}
					// usage 常随最后一个 chunk 携带（include_usage），用于消息头 tokens=
					if (j.usage) {
						botBody.tokIn = j.usage.prompt_tokens | 0;
						botBody.tokOut = j.usage.completion_tokens | 0;
					}
					var ch = j.choices && j.choices[0];
					if (!ch) continue;
					var delta = ch.delta || {};
					if (delta.reasoning_content) botBody.think += delta.reasoning_content;
					else if (delta.reasoning) botBody.think += delta.reasoning;
					if (delta.content) botBody.content += delta.content;
					if (ch.message && ch.message.content) botBody.content += ch.message.content;
					renderStream();
				}
			}
			if (streamErr) finish(null, streamErr);
			else finish();
		} catch (err) {
			if (err && err.name === 'AbortError') {
				// 用户主动停止：保留已生成内容
				state.streaming = false;
				setSending(false);
				renderFinal();
				var durAb = Date.now() - tStart;
				var ttftAb = firstChunkAt ? firstChunkAt - tStart : 0;
				var genAb = durAb - ttftAb;
				if (genAb < 500) genAb = durAb;
				var tpsAb = botBody.tokOut > 0 ? Math.round(botBody.tokOut * 1000 / Math.max(genAb, 1)) : 0;
				insertHeadTime(head, Date.now());
				addHeadMeta(head, durAb, botBody.tokIn, botBody.tokOut, ttftAb, tpsAb);
				state.msgs.push({ q: text, a: botBody.content || '', q_at: tStart, a_at: Date.now(), a_model: state.model, ttft: ttftAb, tps: tpsAb });
				input.value = '';
				autoGrow();
				state.files = [];
				renderFilesBar();
				updateSendBtn();
			} else {
				finish(null, (err && err.message) || '网络错误');
			}
		}
	}

	function setSending(on) {
		$('btn-send').classList.toggle('hidden', on);
		$('btn-stop').classList.toggle('hidden', !on);
		$('input').disabled = false;
	}

	function showSendError(msg) {
		var e = $('send-error');
		e.textContent = msg;
		e.classList.remove('hidden');
		clearTimeout(showSendError._t);
		showSendError._t = setTimeout(hideSendError, 8000);
	}
	function hideSendError() { $('send-error').classList.add('hidden'); }

	/* ---------------- 输入区交互 ---------------- */

	function autoGrow() {
		var t = $('input');
		t.style.height = 'auto';
		t.style.height = Math.min(t.scrollHeight, 180) + 'px';
	}

	function updateSendBtn() {
		$('btn-send').disabled = state.streaming || (!$('input').value.trim() && !state.files.length);
	}

	function wireEvents() {
		$('btn-new').addEventListener('click', function () { newChat(); });

		// 31: 搜索防抖 300ms 后请求后端；✕ 一键清空；71/835: 范围 chips + 日期 + 大小写
		var searchTimer = null;
		var searchInput = function () {
			$('search-clear').classList.toggle('hidden', !$('search').value);
			clearTimeout(searchTimer);
			searchTimer = setTimeout(loadSenList, 300);
		};
		$('search').addEventListener('input', searchInput);
		$('search-clear').addEventListener('click', function () {
			$('search').value = '';
			$('search-clear').classList.add('hidden');
			loadSenList();
			$('search').focus();
		});
		applyChips();
		$('sb-filters').addEventListener('click', function (e) {
			var chip = e.target.closest('.fchip');
			if (!chip) return;
			var k = chip.dataset.scope;
			// 835: 标题退出匹配后，问题/答案 至少保留一个
			if ((k === 'q' || k === 'a') && state.filters[k] && !state.filters[k === 'q' ? 'a' : 'q']) {
				toast('「问题」「答案」至少保留一个');
				return;
			}
			state.filters[k] = !state.filters[k];
			localStorage.setItem(LS_FILT, JSON.stringify(state.filters));
			applyChips();
			loadSenList();
		});
		// 831/832/833: 原生日期控件 + 26/08/15 显示层
		wireDateInput('date-from');
		wireDateInput('date-to');

		// 72: 高亮导航条
		$('hl-prev').addEventListener('click', function () { hlGo(-1); });
		$('hl-next').addEventListener('click', function () { hlGo(1); });
		$('hl-close').addEventListener('click', function () { clearHighlight(); });

		$('model-sel').addEventListener('change', function () {
			state.model = this.value;
			localStorage.setItem(LS_MODEL, state.model);
			var w = $('chat').querySelector('.welcome .w-sub');
			if (w) w.textContent = '当前模型: ' + modelLabel(state.model);
		});

		$('btn-gear').addEventListener('click', function (e) {
			e.stopPropagation();
			$('gear-pop').classList.toggle('hidden');
		});
		var closeGear = function () {
			$('gear-pop').classList.add('hidden');
			// 34: 未确认关闭 → 回退到已保存的系统提示词
			$('sysp').value = state.sysp;
			$('gear-actions').classList.add('hidden');
		};
		document.addEventListener('click', function (e) {
			var pop = $('gear-pop');
			if (!pop.classList.contains('hidden') && !pop.contains(e.target) && e.target !== $('btn-gear')) {
				closeGear();
			}
		});
		// 34: 修改后才出现 确认/取消；确认才保存
		$('sysp').addEventListener('input', function () {
			$('gear-actions').classList.toggle('hidden', this.value === state.sysp);
		});
		$('sysp-ok').addEventListener('click', function () {
			state.sysp = $('sysp').value;
			localStorage.setItem(LS_SYSP, state.sysp);
			$('gear-actions').classList.add('hidden');
			toast('系统提示词已保存');
		});
		$('sysp-cancel').addEventListener('click', closeGear);

		// 32: 手机端开/关抽屉；桌面端收起/展开侧边栏(记住状态)
		$('btn-menu').addEventListener('click', function () {
			if (window.matchMedia('(max-width: 768px)').matches) {
				$('sidebar').classList.toggle('open');
				return;
			}
			var app = $('app-view');
			app.classList.toggle('side-hidden');
			localStorage.setItem('hyb_side', app.classList.contains('side-hidden') ? '0' : '1');
		});
		$('backdrop').addEventListener('click', closeDrawer);

		$('key-badge').addEventListener('click', function () {
			state.includeKey = !state.includeKey;
			var url = new URL(location.href);
			if (state.includeKey) url.searchParams.set('senKey', '1');
			else url.searchParams.delete('senKey');
			history.replaceState(null, '', url);
			updateKeyBadge();
			loadSenList();
		});

		$('btn-attach').addEventListener('click', function () { $('file-input').click(); });
		$('file-input').addEventListener('change', function () {
			addFiles(this.files);
			this.value = '';
		});

		var input = $('input');
		input.addEventListener('input', function () { autoGrow(); updateSendBtn(); });
		input.addEventListener('keydown', function (e) {
			if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
				e.preventDefault();
				send();
			}
		});
		input.addEventListener('paste', function (e) {
			var items = (e.clipboardData && e.clipboardData.files) || [];
			if (items.length) {
				e.preventDefault();
				addFiles(items);
			}
		});

		$('btn-send').addEventListener('click', send);
		$('btn-stop').addEventListener('click', function () {
			if (state.aborter) state.aborter.abort();
		});

		// 灯箱（事件委托：markdown 图片）
		$('chat').addEventListener('click', function (e) {
			var img = e.target.closest ? e.target.closest('img[data-lightbox]') : null;
			if (img) openLightbox(img.src, '');
		});
		$('lb-close').addEventListener('click', closeLightbox);
		$('lightbox').addEventListener('click', function (e) {
			if (e.target === this) closeLightbox();
		});
		document.addEventListener('keydown', function (e) {
			if (e.key === 'Escape') { closeLightbox(); closeDrawer(); clearHighlight(); }
		});

		// 拖拽图片进对话区
		var main = $('main');
		main.addEventListener('dragover', function (e) { e.preventDefault(); });
		main.addEventListener('drop', function (e) {
			e.preventDefault();
			if (e.dataTransfer && e.dataTransfer.files.length) addFiles(e.dataTransfer.files);
		});

		// 是否跟随滚动
		$('chat-scroll').addEventListener('scroll', function () {
			var sc = this;
			state.stick = sc.scrollTop + sc.clientHeight >= sc.scrollHeight - 120;
		});
	}

	function closeDrawer() { $('sidebar').classList.remove('open'); }

	function scrollBottom(instant) {
		var sc = $('chat-scroll');
		if (instant) sc.style.scrollBehavior = 'auto';
		sc.scrollTop = sc.scrollHeight;
		if (instant) setTimeout(function () { sc.style.scrollBehavior = ''; }, 30);
	}

	function openLightbox(src, name) {
		$('lb-img').src = src;
		var dl = $('lb-download');
		dl.href = src;
		dl.setAttribute('download', name || 'image');
		if (/^http/i.test(src) && src.indexOf('/hybapi/file/') >= 0) dl.href = src + (src.indexOf('?') >= 0 ? '&' : '?') + 'dl=1';
		$('lightbox').classList.remove('hidden');
	}
	function closeLightbox() {
		$('lightbox').classList.add('hidden');
		$('lb-img').src = '';
	}

	function updateKeyBadge() {
		$('key-badge').classList.toggle('hidden', !state.includeKey);
		$('key-badge').classList.toggle('force-show', state.includeKey);
		$('key-badge').textContent = state.includeKey ? '含 API 密钥会话 ✓' : '含 API 密钥会话';
	}

	/* ---------------- 启动 ---------------- */

	updateKeyBadge();
	if (state.token) {
		apiJSON('/hybapi/me')
			.then(function (d) {
				state.user = d.user;
				showApp();
				return initApp();
			})
			.catch(function () { /* 已切回登录页 */ });
	} else {
		showLogin();
	}
})();

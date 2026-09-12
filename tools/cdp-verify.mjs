// 用 CDP 驱动 Edge，验证前端在**真实浏览器**里的行为。
//
// 为什么用 Node 而不是装 playwright/puppeteer：Node 22+ 自带全局 WebSocket，
// 而 CDP 本身就是「HTTP 拿目标列表 + WebSocket 发 JSON 命令」，不需要任何依赖。
// 这个项目刚被"隐藏依赖"坑过一次（native 依赖 VC++ 运行库），能不引就不引。
//
// 用法：
//   node tools/cdp-verify.mjs <cdp-port> <app-origin>
//   node tools/cdp-verify.mjs 9333 http://127.0.0.1:8799
//
// 退出码：0 = 全部通过；1 = 有失败；2 = 连不上 CDP。

const [, , cdpPort = '9333', appOrigin = 'http://127.0.0.1:8799'] = process.argv;

async function targets() {
  const r = await fetch(`http://127.0.0.1:${cdpPort}/json`);
  return (await r.json()).filter(t => t.type === 'page');
}

async function connect(wsUrl) {
  const ws = new WebSocket(wsUrl);
  await new Promise((ok, no) => {
    ws.onopen = ok;
    ws.onerror = () => no(new Error('WebSocket 连接失败（需要 --remote-allow-origins=* 吗？）'));
  });
  let id = 0;
  const pending = new Map();
  const events = [];
  ws.onmessage = ev => {
    const m = JSON.parse(ev.data);
    if (m.id && pending.has(m.id)) {
      const { ok, no } = pending.get(m.id);
      pending.delete(m.id);
      m.error ? no(new Error(JSON.stringify(m.error))) : ok(m.result);
    } else if (m.method) {
      events.push(m);
    }
  };
  return {
    events,
    send(method, params = {}) {
      const myId = ++id;
      return new Promise((ok, no) => {
        pending.set(myId, { ok, no });
        ws.send(JSON.stringify({ id: myId, method, params }));
        setTimeout(() => {
          if (pending.has(myId)) { pending.delete(myId); no(new Error(`超时: ${method}`)); }
        }, 15000);
      });
    },
    close() { ws.close(); },
  };
}

async function evaluate(c, expr) {
  const r = await c.send('Runtime.evaluate', {
    expression: expr, returnByValue: true, awaitPromise: true,
  });
  if (r.exceptionDetails) throw new Error('页面内异常: ' + JSON.stringify(r.exceptionDetails));
  return r.result.value;
}

const results = [];
function check(name, pass, detail) {
  results.push({ name, pass, detail });
  console.log(`${pass ? 'PASS' : 'FAIL'}  ${name}${detail ? '  — ' + detail : ''}`);
}

// ── 连上目标页 ────────────────────────────────────────────────
const pages = await targets();
const appPage = pages.find(t => t.url.startsWith(appOrigin));
if (!appPage) {
  console.error(`找不到 ${appOrigin} 的页面目标。现有目标：`);
  pages.forEach(t => console.error(`  ${t.url}`));
  process.exit(2);
}
console.log(`目标页：${appPage.url}\n`);

const c = await connect(appPage.webSocketDebuggerUrl);
await c.send('Runtime.enable');
await c.send('Log.enable');       // 收集浏览器日志（含网络/安全错误）
await c.send('Page.enable');

// 先**强制重新导航**到悬浮窗：脚本读的是导航之后的页面，不依赖 Edge 里
// 之前恰好停在什么状态。（初版没做这一步，结果读到的是上一次遗留的错误页，
// 一次就误判了四项。）
await c.send('Page.navigate', { url: `${appOrigin}/?page=overlay` });
await new Promise(r => setTimeout(r, 4000));
c.events.length = 0;              // 清掉导航前的旧事件

// ── ① 页面有内容（不是白屏）────────────────────────────────
const bodyLen = await evaluate(c, 'document.body.innerText.trim().length');
check('悬浮窗渲染出内容', bodyLen > 0, `innerText 长度 ${bodyLen}`);

// ── ② SSE 真的连上了：直接问后端订阅者数 ───────────────────
// 页面内拿不到 EventSource 实例（前端存在模块作用域里），所以用后端视角判定：
// 前端页面加载后必须自己发起 SSE 订阅，sseClients ≥ 1 就是"页面真的跑起来了"
// 的硬证据——比"innerText 非空"强得多（错误页的 innerText 也非空）。
const health = async () => (await (await fetch(`${appOrigin}/api/health`)).json()).data;
const h = await health();
check('页面连上了 SSE', h.sseClients >= 1, `sseClients = ${h.sseClients}`);

// ── ③ 页面加载时拉了消息快照（F5 不清空的**机制**）─────────
// useMessages.ts:155 说 resyncMessages() 注册在首次连接上，所以每次页面加载
// 都会 GET /api/messages。
//
// ⚠️ 这一项验证的是**机制**，不是"F5 之后消息真的还在"——后者需要至少一条
// 真实聊天消息，而本脚本**不往用户的聊天日志里写东西**（那是用户数据）。
// 两者在证据文档里要分开写，别把机制当成行为。
const resources = await evaluate(c, 'performance.getEntriesByType("resource").map(e => e.name)');
const snapshotCalls = resources.filter(u => u.includes('/api/messages'));
check('加载时拉取了消息快照', snapshotCalls.length >= 1,
  snapshotCalls.length ? `${snapshotCalls.length} 次` : '**一次都没拉**');

// ── ③ 设置页：导航项 ───────────────────────────────────────
await c.send('Page.navigate', { url: `${appOrigin}/?page=settings` });
await new Promise(r => setTimeout(r, 3000));

const navSel = await evaluate(c, `(() => {
  const cands = ['nav button', 'nav a', '.nav-item', '[class*=nav] button', '[class*=nav] a'];
  for (const s of cands) {
    const n = document.querySelectorAll(s).length;
    if (n >= 5) return { sel: s, n };
  }
  return { sel: null, n: 0 };
})()`);
check('设置页有导航项', navSel.n >= 5, navSel.sel ? `${navSel.sel} × ${navSel.n}` : '没找到匹配的选择器');

// ── ④ 密钥框那句假提示已消失（本轮修复的核心）──────────────
const hintLeak = await evaluate(c, 'document.body.innerText.includes("留空则不修改")');
check('密钥框假提示已消失', hintLeak === false, hintLeak ? '**仍然出现**' : '未出现');

const hasKeyInput = await evaluate(c, '!!document.querySelector("input[type=password]")');
check('密钥输入框存在', hasKeyInput === true);

// ── ⑤ 页面有没有报错（诊断用，不算判据）────────────────────
const errs = [
  ...c.events.filter(e => e.method === 'Log.entryAdded' && e.params.entry.level === 'error')
    .map(e => e.params.entry.text),
  ...c.events.filter(e => e.method === 'Runtime.exceptionThrown')
    .map(e => e.params.exceptionDetails.text),
];
console.log(`\n控制台错误 ${errs.length} 条：`);
errs.slice(0, 10).forEach(t => console.log('  · ' + t.slice(0, 160)));

c.close();
const failed = results.filter(r => !r.pass);
console.log(`\n合计 ${results.length} 项，失败 ${failed.length} 项`);
process.exit(failed.length ? 1 : 0);

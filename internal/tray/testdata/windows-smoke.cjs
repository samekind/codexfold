// Run after building the tray: node internal/tray/testdata/windows-smoke.cjs <exe> [screenshots-dir] [live-store]
// Exercises the real embedded WebView2 through a temporary loopback debug port.
// No desktop automation, extra npm dependencies, or production store is used.
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const net = require('node:net');
const path = require('node:path');
const {spawn} = require('node:child_process');

const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(action, label) {
  const deadline = Date.now() + 15000;
  let last;
  while (Date.now() < deadline) {
    try { const result = await action(); if (result) return result; } catch (error) { last = error; }
    await pause(150);
  }
  throw new Error(label + ' timed out' + (last ? ': ' + last.message : ''));
}
async function freePort() {
  const server = net.createServer();
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const port = server.address().port;
  await new Promise(resolve => server.close(resolve));
  return port;
}
async function connect(port) {
  const target = await until(async () => {
    const response = await fetch('http://127.0.0.1:' + port + '/json/list', {signal: AbortSignal.timeout(1000)});
    return (await response.json()).find(item => item.title === 'CodexFold');
  }, 'WebView2 page');
  const socket = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((resolve, reject) => { socket.onopen = resolve; socket.onerror = reject; });
  let serial = 0;
  const pending = new Map(), exceptions = [];
  socket.onmessage = event => {
    const result = JSON.parse(event.data);
    if (result.method === 'Runtime.exceptionThrown') exceptions.push(result.params.exceptionDetails);
    const request = pending.get(result.id);
    if (!request) return;
    pending.delete(result.id); clearTimeout(request.timer);
    result.error ? request.reject(new Error(JSON.stringify(result.error))) : request.resolve(result.result);
  };
  const call = (method, params = {}) => new Promise((resolve, reject) => {
    const id = ++serial;
    const timer = setTimeout(() => { pending.delete(id); reject(new Error(method + ' timed out')); }, 8000);
    pending.set(id, {resolve, reject, timer}); socket.send(JSON.stringify({id, method, params}));
  });
  const evaluate = async expression => {
    const result = await call('Runtime.evaluate', {expression, returnByValue: true, awaitPromise: true});
    if (result.exceptionDetails) throw new Error(JSON.stringify(result.exceptionDetails));
    return result.result.value;
  };
  await call('Runtime.enable');
  return {call, evaluate, exceptions, close: () => socket.close()};
}
async function main() {
  assert.equal(process.platform, 'win32', 'This test requires Windows and WebView2 Runtime');
  assert.ok(process.argv[2], 'Provide the built tray executable');
  const executable = path.resolve(process.argv[2]);
  const screenshots = process.argv[3] && path.resolve(process.argv[3]);
  const temporaryRoot = path.resolve(process.env.CODEXFOLD_TEST_TEMP || path.join(process.cwd(), '.tmp'));
  await fs.mkdir(temporaryRoot, {recursive:true});
  const root = await fs.mkdtemp(path.join(temporaryRoot, 'codexfold-tray-smoke-'));
  const port = await freePort();
  const liveStore = process.argv[4] && path.resolve(process.argv[4]);
  const store = liveStore || path.join(root, 'store');
  if (!liveStore) {
    await fs.mkdir(path.join(store,'enrollment'), {recursive:true});
    await fs.writeFile(path.join(store,'enrollment/ui-history-v1.json'), JSON.stringify({version:1,samples:[{t:Date.now()-20*86400000,l:10000000,p:6000000,health:'运行正常'}],incidents:[]}));
  }
  const env = {...process.env, WEBVIEW2_USER_DATA_FOLDER: path.join(root, 'webview'), WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS: '--remote-debugging-port=' + port};
  // windowsHide supplies STARTF_USESHOWWINDOW/SW_HIDE, the failing launcher condition.
  const child = spawn(executable, ['--store', store], {env, windowsHide: true, stdio: 'ignore'});
  let client;
  try {
    child.on('error', error => { console.error(error); });
    client = await connect(port);
    if (liveStore) {
      await until(() => client.evaluate("document.visibilityState === 'visible' && document.getElementById('health')?.textContent === '运行正常'"), 'Live filesystem dashboard');
      const status = await client.evaluate("({health:document.getElementById('health').textContent,sessions:document.getElementById('session-count').textContent,logical:document.getElementById('logical').textContent,physical:document.getElementById('physical').textContent,daemon:document.getElementById('daemon-state').textContent,managed:document.getElementById('managed-state').textContent,enrollment:document.getElementById('fold-status').textContent,overflow:document.documentElement.scrollWidth > innerWidth})");
      assert.equal(status.daemon, '运行正常');
      assert.equal(status.managed, '运行正常');
      assert.match(status.sessions, /托管会话：\d+/);
      assert.notEqual(status.logical, '—');
      assert.notEqual(status.physical, '—');
      assert.equal(status.overflow, false);
      if (process.env.CODEXFOLD_EXPECT_ACCOUNTING === '1') {
        const end = Date.now()+60000;
        while (await client.evaluate("document.getElementById('saved').textContent === '—'") && Date.now()<end) await pause(250);
        const accounting = await client.evaluate("({saved:document.getElementById('saved').textContent,pending:document.getElementById('pending').textContent,netLabel:document.getElementById('net-label').textContent,net:document.getElementById('net-saved').textContent})");
        assert.notEqual(accounting.saved,'—'); assert.notEqual(accounting.pending,'—'); assert.notEqual(accounting.net,'—');
        console.log('PASS: live compression, retained occupancy and signed net accounting',accounting);
      }
      if (process.env.CODEXFOLD_EXPECT_ENROLLMENT_ACTIVE === '1') {
        assert.ok(['等待下次检查','正在检查','正在折叠','正在打包','正在迁移','正在回收','等待回收'].includes(status.enrollment), 'Live enrollment must be enabled without an error');
      }
      if (screenshots) {
        await fs.mkdir(screenshots, {recursive: true});
        const capture = await client.call('Page.captureScreenshot', {format:'png',captureBeyondViewport:false});
        await fs.writeFile(path.join(screenshots, 'dashboard-live.png'), Buffer.from(capture.data,'base64'));
      }
      assert.deepEqual(client.exceptions, []);
      console.log('PASS: native tray receives live filesystem health, session count and storage metrics', status);
      await client.evaluate("window.chrome.webview.postMessage('exit')");
      await until(() => child.exitCode !== null, 'Live tray exit');
      assert.equal(child.exitCode, 0);
      return;
    }
    await until(() => client.evaluate("document.readyState === 'complete' && document.visibilityState === 'visible' && document.getElementById('health')?.textContent === '未连接'"), 'First-launch visible dashboard');
    console.log('PASS: first launch renders with a hidden launcher; native status bridge responds');
    const enrollment = path.join(store, 'enrollment');
    await fs.mkdir(enrollment, {recursive:true});
    await fs.writeFile(path.join(enrollment,'worker-policy.json'),JSON.stringify({version:1,enabled:true,interval:'1m',stable_for:'1h',archived_only:false,batch_size:5}));
    for (const [phase,enabled,label] of [['waiting-filesystem',true,'等待文件系统'],['stopped',true,'后台已停止'],['config-invalid',false,'设置有误'],['disabled',false,'已关闭']]) {
      await fs.writeFile(path.join(enrollment,'worker-status.json'),JSON.stringify({version:1,store_path:store,enabled,phase,interval:'1m',stable_for:'1h',archived_only:false,managed_count:0,waiting_count:0,waiting_known:false,cycle_total:0,cycle_done:0,updated_at:new Date().toISOString()}));
      await client.evaluate("window.chrome.webview.postMessage('refresh')");
      await until(()=>client.evaluate("document.getElementById('fold-status').textContent === "+JSON.stringify(label)),label);
    }
    // Keep the longer waiting label visible in all existing size checks.
    await fs.writeFile(path.join(enrollment,'worker-status.json'),JSON.stringify({version:1,store_path:store,enabled:true,phase:'waiting-filesystem',managed_count:0,waiting_count:0,waiting_known:false,cycle_total:0,cycle_done:0,updated_at:new Date().toISOString()}));
    await client.evaluate("window.chrome.webview.postMessage('refresh')");
    await until(()=>client.evaluate("document.getElementById('fold-status').textContent === '等待文件系统'"),'Waiting label');
    console.log('PASS: persistent worker waiting, stopped, invalid and user-paused states render without a mount');
    assert.ok(await client.evaluate('samples.some(item=>Date.now()-item.t>7*86400000)'), 'Persisted history must survive loading');
    assert.equal(await client.evaluate("getComputedStyle(document.getElementById('policy-enabled')).width"), '64px');
    await until(()=>client.evaluate("getComputedStyle(document.querySelector('.glass')).backdropFilter.includes('url(')"), 'First visible glass lens');
    await client.evaluate("document.getElementById('policy-batch').value='25';document.getElementById('policy-batch').dispatchEvent(new Event('change',{bubbles:true}))");
    await until(()=>client.evaluate('!pendingPolicy'), 'Native policy save');
    let savedPolicy=JSON.parse(await fs.readFile(path.join(enrollment,'worker-policy.json'),'utf8'));
    assert.equal(savedPolicy.batch_size,25);assert.equal(savedPolicy.interval,'1m0s');assert.equal(savedPolicy.stable_for,'1h0m0s');
    await client.evaluate("document.getElementById('policy-enabled').click()");
    await until(()=>client.evaluate('!pendingPolicy && !document.getElementById("policy-enabled").checked'), 'Pause setting');
    savedPolicy=JSON.parse(await fs.readFile(path.join(enrollment,'worker-policy.json'),'utf8'));assert.equal(savedPolicy.enabled,false);
    console.log('PASS: policy edits preserve other fields; pause persists; 30-day history loads; glass lens and switch proportions render');
    // Only this isolated test page uses metric fixtures. Shipping status still
    // arrives from native read-only accounting; no production files are changed.
    const metrics = {LogicalBytes:916715330,PhysicalBytes:1698632426,CompressionLogicalBytes:916715330,CompressedBytes:604913040,PendingBytes:1087218322};
    await client.evaluate(`(() => {
      const nativeUpdate = window.updateStatus;
      window.metricFixture = ${JSON.stringify(metrics)};
      window.nativeUpdates = 0;
      window.updateStatus = view => { window.nativeUpdates++; nativeUpdate({...view,...window.metricFixture}); };
    })()`);
    await client.evaluate("window.chrome.webview.postMessage('refresh')");
    await until(()=>client.evaluate("document.getElementById('saved').textContent === '311.8 MB'"),'Compression saving');
    let metricState = await client.evaluate("({saved:document.getElementById('saved').textContent,pending:document.getElementById('pending').textContent,label:document.getElementById('net-label').textContent,net:document.getElementById('net-saved').textContent})");
    assert.deepEqual(metricState,{saved:'311.8 MB',pending:'1.1 GB',label:'暂多占',net:'781.9 MB'});
    await client.evaluate("window.metricFixture.PhysicalBytes = 650000000; window.chrome.webview.postMessage('refresh')");
    await until(()=>client.evaluate("document.getElementById('net-label').textContent === '当前净节省'"),'Positive net saving');
    assert.equal(await client.evaluate("document.getElementById('saved').textContent"),'311.8 MB');
    await client.evaluate("window.metricFixture.PhysicalBytes = 1698632426; window.chrome.webview.postMessage('refresh')");
    await until(()=>client.evaluate("document.getElementById('net-label').textContent === '暂多占'"),'Negative net saving');
    console.log('PASS: compression stays visible while temporary storage changes net saving');
    for (const [width, height] of [[1060, 730], [540, 420], [390, 730]]) {
      await client.call('Emulation.setDeviceMetricsOverride', {width, height, deviceScaleFactor: 1, mobile: false});
      const layout = await client.evaluate(`({width:innerWidth,scroll:document.documentElement.scrollWidth,columns:getComputedStyle(document.querySelector('.metrics')).gridTemplateColumns.split(' ').length,heading:document.querySelector('h1').textContent})`);
      assert.ok(layout.scroll <= layout.width, 'Horizontal overflow at ' + width);
      assert.equal(layout.columns, width > 720 ? 4 : 2);
      assert.equal(layout.heading, 'CodexFold');
      const scroll = await client.evaluate(`(() => {
        const content = document.getElementById('content-scroll');
        const headerTop = document.querySelector('.toolbar').getBoundingClientRect().top;
        content.scrollTo(0, content.scrollHeight);
        const result = {top:content.scrollTop,root:document.scrollingElement.scrollTop,headerTop,headerAfter:document.querySelector('.toolbar').getBoundingClientRect().top,scrollbar:getComputedStyle(content,'::-webkit-scrollbar').width,overflow:content.scrollWidth > content.clientWidth};
        content.scrollTo(0,0); return result;
      })()`);
      assert.ok(scroll.top > 0, 'Content must remain scrollable');
      assert.equal(scroll.root, 0, 'Window itself must not scroll');
      assert.equal(scroll.headerAfter, scroll.headerTop, 'Toolbar moved with content');
      assert.equal(scroll.scrollbar, '10px');
      assert.equal(scroll.overflow, false, 'Horizontal content overflow');
      if (screenshots && width !== 540) {
        await fs.mkdir(screenshots, {recursive: true});
        const capture = await client.call('Page.captureScreenshot', {format: 'png', captureBeyondViewport: false});
        await fs.writeFile(path.join(screenshots, 'dashboard-' + width + '.png'), Buffer.from(capture.data, 'base64'));
      }
    }
    await client.call('Emulation.clearDeviceMetricsOverride');
    const wheelPoint = await client.evaluate("({x:innerWidth/2,y:document.querySelector('.toolbar').getBoundingClientRect().bottom+80})");
    await client.call('Input.dispatchMouseEvent', {type:'mouseWheel', ...wheelPoint, deltaX:0, deltaY:360});
    await until(() => client.evaluate("document.getElementById('content-scroll').scrollTop > 0"), 'Mouse wheel scrolling');
    await client.evaluate("document.getElementById('content-scroll').scrollTo(0,0); document.getElementById('content-scroll').focus()");
    await client.call('Input.dispatchKeyEvent', {type:'rawKeyDown', key:'PageDown', code:'PageDown', windowsVirtualKeyCode:34});
    await client.call('Input.dispatchKeyEvent', {type:'keyUp', key:'PageDown', code:'PageDown', windowsVirtualKeyCode:34});
    await until(() => client.evaluate("document.getElementById('content-scroll').scrollTop > 0"), 'Keyboard scrolling');
    await client.evaluate("document.getElementById('content-scroll').scrollTo(0,0)");
    console.log('PASS: actual mouse wheel and PageDown scroll the content region');
    assert.equal(await client.evaluate("document.querySelector('[data-hours=\"168\"]').click(); document.getElementById('range-start').textContent"), '7 天前');
    assert.equal(await client.evaluate("document.getElementById('show-diagnostics').click(); document.getElementById('diagnostics').open"), true);
    assert.equal(await client.evaluate("document.getElementById('close-diagnostics').click(); document.getElementById('diagnostics').open"), false);
    assert.equal(await client.evaluate("document.getElementById('more').click(); document.getElementById('menu').hidden"), false);
    assert.equal(await client.evaluate("document.querySelector('main').click(); document.getElementById('menu').hidden"), true);
    console.log('PASS: desktop/narrow/short layout, fixed toolbar, slim content scrollbar, time range, diagnostics and menu');
    const before = await client.evaluate('window.nativeUpdates');
    await client.evaluate("document.querySelector('[data-command=\"refresh\"]').click()");
    await until(async () => await client.evaluate('window.nativeUpdates') > before, 'Native refresh');
    assert.deepEqual(client.exceptions, []);
    console.log('PASS: refresh reaches native host; no JavaScript exceptions');
    await client.evaluate("window.chrome.webview.postMessage('exit')");
    await until(() => child.exitCode !== null, 'Native exit');
    assert.equal(child.exitCode, 0);
    client.close(); client = null;
    console.log('PASS: exit button shuts down the tray process');

    // Also check a background start, followed by exactly one existing-instance open.
    const background = spawn(executable, ['--background', '--store', store], {env, windowsHide: true, stdio: 'ignore'});
    try {
      client = await connect(port);
      assert.equal(await client.evaluate('document.visibilityState'), 'hidden');
      const opener = spawn(executable, ['--store', store], {env, windowsHide: true, stdio: 'ignore'});
      await until(() => opener.exitCode !== null, 'Existing-instance opener');
      assert.equal(opener.exitCode, 0);
      await until(() => client.evaluate("document.visibilityState === 'visible'"), 'Background window restore');
      const quitter = spawn(executable, ['--quit', '--store', store], {env, windowsHide: true, stdio: 'ignore'});
      await until(() => quitter.exitCode !== null, 'Tray-only quit command');
      assert.equal(quitter.exitCode, 0);
      await until(() => background.exitCode !== null, 'Background process exit');
      assert.equal(background.exitCode, 0);
      console.log('PASS: background start stays hidden; one open restores the dashboard');
    } finally { if (background.exitCode === null) background.kill(); }
  } finally {
    client?.close();
    if (child.exitCode === null) child.kill();
    // Remove only the exact temporary directory created by this test.
    assert.equal(path.dirname(path.resolve(root)), temporaryRoot);
    assert.ok(path.basename(root).startsWith('codexfold-tray-smoke-'));
    await fs.rm(root, {recursive: true, force: true, maxRetries: 20, retryDelay: 250});
  }
}
main().catch(error => { console.error(error); process.exitCode = 1; });

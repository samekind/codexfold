// A real, short model conversation in the owned Windows fixture. Authentication
// is supplied over stdin in memory; production history and credentials stay put.
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');
const readline = require('node:readline');
const {spawn} = require('node:child_process');
const {randomBytes} = require('node:crypto');

async function main() {
  assert.equal(process.platform, 'win32');
  const executable = path.resolve(process.argv[2] || '');
  const root = path.resolve(process.argv[3] || '');
  const mode = process.argv[4] || 'read';
  assert.ok(['start', 'resume', 'recovered', 'live-recovery', 'read'].includes(mode));
  assert.ok(root.startsWith(path.resolve(__dirname, '..', '.tmp', 'windows-use') + path.sep));
  const fixture = JSON.parse(await fs.readFile(path.join(root, 'full-fixture.json'), 'utf8'));
  const home = path.resolve(fixture.home);
  assert.ok(home.startsWith(root + path.sep));
  const recordPath = path.join(root, 'model-canary.json');
  const cwd = path.join(root, 'model-work');
  await fs.mkdir(cwd, {recursive:true});
  await assert.rejects(fs.stat(path.join(home, 'auth.json')), e => e.code === 'ENOENT');
  let record = mode === 'start' ? {code:'CF_' + randomBytes(10).toString('hex'), phases:[]} : JSON.parse(await fs.readFile(recordPath, 'utf8'));
  const auth = mode === 'read' ? null : JSON.parse(await fs.readFile(path.join(process.env.USERPROFILE, '.codex', 'auth.json'), 'utf8'));
  if (auth) assert.ok(auth.tokens?.access_token && auth.tokens?.account_id, 'Existing ChatGPT login is required');
  const child = spawn(executable, ['app-server', '--stdio'], {
    cwd, env:{...process.env, CODEX_HOME:home}, windowsHide:true, stdio:['pipe','pipe','pipe'],
  });
  const pending = new Map(), completed = new Map(), waiters = new Map();
  let serial = 0, tools = 0, stderr = '';
  const send = message => child.stdin.write(JSON.stringify(message) + '\n');
  child.stderr.on('data', chunk => { stderr += chunk.toString(); });
  const lines = readline.createInterface({input:child.stdout});
  lines.on('line', line => {
    let m; try { m = JSON.parse(line); } catch { return; }
    if (m.id !== undefined && m.method) {
      send({id:m.id,error:{code:-32601,message:'Test client declines external actions and token refresh'}});
      return;
    }
    if (m.method === 'item/started' && !['userMessage','agentMessage','reasoning'].includes(m.params?.item?.type)) tools++;
    if (m.method === 'turn/completed') {
      const key = m.params.threadId;
      completed.set(key, m.params.turn);
      const waiter = waiters.get(key);
      if (waiter) { clearTimeout(waiter.timer); waiters.delete(key); waiter.resolve(m.params.turn); }
    }
    const request = pending.get(m.id);
    if (!request) return;
    clearTimeout(request.timer); pending.delete(m.id);
    if (m.error) request.reject(new Error(request.method + ': ' + String(m.error.message)));
    else request.resolve(m.result);
  });
  child.on('exit', () => {
    for (const request of [...pending.values(), ...waiters.values()]) { clearTimeout(request.timer); request.reject(new Error('Owned model test client exited')); }
    pending.clear(); waiters.clear();
  });
  const call = (method, params) => new Promise((resolve,reject) => {
    const id = ++serial;
    const timer = setTimeout(() => { pending.delete(id); reject(new Error(method + ' timed out')); }, 60000);
    pending.set(id,{method,resolve,reject,timer}); send({id,method,params});
  });
  const waitTurn = threadId => new Promise((resolve,reject) => {
    if (completed.has(threadId)) return resolve(completed.get(threadId));
    const timer = setTimeout(() => { waiters.delete(threadId); reject(new Error('Real model turn timed out')); },180000);
    waiters.set(threadId,{resolve,reject,timer});
  });
  const options = {cwd,model:'gpt-6.1-sol',modelProvider:'openai',approvalPolicy:'never',sandbox:'read-only',
    baseInstructions:'You are a storage compatibility test. Answer only the requested short text. Do not use any tools, access files, or delegate.',
    config:{model_reasoning_effort:'medium'},environments:[]};
  try {
    await call('initialize',{clientInfo:{name:'codexfold_windows_model_canary',version:'1.0.0'},capabilities:{experimentalApi:true}});
    send({method:'initialized',params:{}});
    if (auth) {
      await call('account/login/start',{type:'chatgptAuthTokens',accessToken:auth.tokens.access_token,chatgptAccountId:auth.tokens.account_id});
      console.log('PASS: isolated test client authenticated in memory');
    }
    let thread;
    if (mode === 'start') thread = (await call('thread/start',options)).thread;
    else {
      assert.ok(record.id && record.path);
      const canaryPath = path.resolve(String(record.path).replace(/^\\\\\?\\/,''));
      assert.ok(canaryPath.toLowerCase().startsWith(home.toLowerCase() + path.sep), 'Resume only the isolated canary');
      record.path = canaryPath;
      if (mode !== 'read') await assert.rejects(fs.stat(path.join(root,'native',path.relative(home,record.path))), e => e.code === 'ENOENT');
      thread = (await call('thread/resume',{...options,threadId:record.id,path:record.path})).thread;
    }
    if (mode === 'live-recovery') {
      const trigger = path.join(root,'model-recovery-trigger.json');
      await fs.rm(trigger,{force:true});
      await fs.writeFile(path.join(root,'model-recovery-ready.json'),JSON.stringify({threadId:thread.id,pid:child.pid}));
      console.log('READY: owned model test client remains open during filesystem recovery');
      const deadline = Date.now() + 180000;
      while (true) {
        try { await fs.access(trigger); break; } catch (error) { if (error.code !== 'ENOENT') throw error; }
        assert.ok(Date.now() < deadline,'Filesystem recovery signal timed out');
        await new Promise(resolve => setTimeout(resolve,250));
      }
    }
    if (mode !== 'read') {
      const prompt = mode === 'start' ? 'Remember the code ' + record.code + '. Reply with exactly SAVED.' :
        mode === 'resume' ? 'What code did I ask you to remember? Reply only with that code.' :
          'Reply with the code I asked you to remember, followed by a space and RECOVERED.';
      completed.delete(thread.id);
      const waiting = waitTurn(thread.id);
      // Attach a handler immediately, including when turn/start itself fails.
      waiting.catch(() => {});
      const started = await call('turn/start',{threadId:thread.id,input:[{type:'text',text:prompt}],effort:'medium',environments:[]});
      console.log('Started real model turn: ' + mode);
      const turn = await waiting;
      assert.equal(turn.id,started.turn.id);
      assert.equal(turn.status,'completed', 'The real model turn must finish successfully');
      const text = turn.items.filter(item => item.type === 'agentMessage').map(item => item.text).join('\n').trim();
      assert.equal(text, mode === 'start' ? 'SAVED' : mode === 'resume' ? record.code : record.code + ' RECOVERED');
      assert.equal(tools,0,'The model must not invoke tools');
      record.phases.push({mode,turnId:turn.id,completed:true});
      console.log('PASS: real model replied correctly without tool actions (' + mode + ')');
    }
    const read = (await call('thread/read',{threadId:thread.id,includeTurns:true})).thread;
    record.id = read.id; record.path = read.path;
    // Paths from the app-server may be verbatim drive paths. Validate their
    // resolved namespace before persisting the record or reading any bytes.
    const localPath = path.resolve(String(record.path).replace(/^\\\\\?\\/,''));
    assert.ok(localPath.toLowerCase().startsWith(home.toLowerCase() + path.sep), 'The canary must remain in the isolated home');
    record.path = localPath;
    const messages = read.turns.flatMap(turn => turn.items).filter(item => item.type === 'agentMessage').map(item => item.text.trim());
    for (const phase of record.phases) {
      const expected = phase.mode === 'start' ? 'SAVED' : phase.mode === 'resume' ? record.code : record.code + ' RECOVERED';
      assert.ok(messages.includes(expected),'Previously completed replies must survive reopening');
    }
    record.visibleTurns = read.turns.length;
    await fs.writeFile(recordPath,JSON.stringify(record,null,2));
    console.log('PASS: persisted history contains every completed reply; turns=' + read.turns.length);
  } finally {
    child.stdin.end();
    await new Promise(resolve => {
      if (child.exitCode !== null) return resolve();
      const timer = setTimeout(() => { child.kill(); resolve(); },5000);
      child.once('exit', () => { clearTimeout(timer); resolve(); });
    });
    lines.close();
    for (const request of [...pending.values(), ...waiters.values()]) clearTimeout(request.timer);
    // Keep diagnostic text private and remove token values before writing it.
    if (auth) for (const token of Object.values(auth.tokens)) if (typeof token === 'string' && token) stderr = stderr.split(token).join('[REDACTED]');
    await fs.writeFile(path.join(root,'model-client.stderr.log'),stderr);
    await assert.rejects(fs.stat(path.join(home,'auth.json')), e => e.code === 'ENOENT','External-token authentication must not create a credential file');
  }
}
main().catch(error => { console.error(error.message); process.exitCode = 1; });

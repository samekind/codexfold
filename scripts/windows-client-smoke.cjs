// Unmodified local Codex app-server against an explicitly isolated Windows fixture.
// This stage sends no model requests and never copies login credentials.
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');
const readline = require('node:readline');
const {spawn} = require('node:child_process');

async function main() {
  assert.equal(process.platform, 'win32');
  const executable = path.resolve(process.argv[2] || '');
  const root = path.resolve(process.argv[3] || '');
  const testBase = path.resolve(__dirname, '..', '.tmp', 'windows-use') + path.sep;
  assert.ok(root.startsWith(testBase), 'Only workspace-owned isolated fixtures are accepted');
  let full = true;
  try { await fs.access(path.join(root, 'full-fixture.json')); } catch { full = false; }
  const fixture = JSON.parse(await fs.readFile(path.join(root, full ? 'full-fixture.json' : 'fixture.json'), 'utf8'));
  const deletionCanary = process.argv[4] === 'delete-fork';
  const forkFixture = deletionCanary && JSON.parse(await fs.readFile(path.join(root, 'client-fork.json'), 'utf8'));
  const session = deletionCanary ? {id:forkFixture.id,rollout_path:forkFixture.path} : full ? fixture.session : fixture.sessions[process.argv[4] === 'native' ? 1 : 0];
  const managed = process.argv[4] === 'managed' || deletionCanary;
  let mounted = full ? session.rollout_path : path.join(fixture.mount, session.id + '.jsonl');
  // Let the real client create its own complete schema, rather than using the
  // small engine-only fixture database. No production database is copied.
  const clientHome = full ? fixture.home : path.join(root, 'client-home');
  assert.ok(path.resolve(clientHome).toLowerCase().startsWith(root.toLowerCase() + path.sep), 'The test home must remain inside its owned fixture');
  if (full) assert.ok(path.resolve(mounted).toLowerCase().startsWith(path.resolve(clientHome).toLowerCase() + path.sep), 'The selected rollout must belong to the copied home');
  if (managed) {
    assert.ok(full, 'Managed client validation requires a complete fixture');
    const native = path.join(root, 'native', path.relative(clientHome, mounted));
    await assert.rejects(fs.stat(native), error => error.code === 'ENOENT', 'The managed rollout must be served from compressed storage');
  }
  await fs.mkdir(clientHome, {recursive:true});
  if (!full && process.argv[4] === 'native') {
    const date = path.basename(session.source_path).match(/^rollout-(\d{4})-(\d{2})-(\d{2})T/);
    assert.ok(date, 'The native baseline must preserve the original rollout filename');
    const directory = path.join(clientHome, 'sessions', date[1], date[2], date[3]);
    await fs.mkdir(directory, {recursive:true});
    mounted = path.join(directory, path.basename(session.source_path));
    await fs.copyFile(session.path, mounted);
  }
  const child = spawn(executable, ['app-server', '--stdio'], {
    env: {...process.env, CODEX_HOME: clientHome}, cwd: root,
    windowsHide: true, stdio: ['pipe', 'pipe', 'pipe'],
  });
  const pending = new Map();
  let serial = 0, stderr = '';
  child.stderr.on('data', chunk => { stderr += chunk.toString(); });
  const lines = readline.createInterface({input: child.stdout});
  lines.on('line', line => {
    let message;
    try { message = JSON.parse(line); } catch { return; }
    if (message.id !== undefined && message.method) {
      child.stdin.write(JSON.stringify({id:message.id, error:{code:-32601,message:'No test approval or external action handler'}}) + '\n');
      return;
    }
    const request = pending.get(message.id);
    if (!request) return;
    clearTimeout(request.timer); pending.delete(message.id);
    if (message.error) {
      fs.writeFile(path.join(root, 'client-error.json'), JSON.stringify(message.error, null, 2)).catch(() => {});
      request.reject(new Error(request.method + ' failed with code ' + message.error.code));
    } else request.resolve(message.result);
  });
  child.on('exit', () => {
    for (const request of pending.values()) { clearTimeout(request.timer); request.reject(new Error('Owned test app-server exited')); }
    pending.clear();
  });
  const call = (method, params) => new Promise((resolve, reject) => {
    const id = ++serial;
    const timer = setTimeout(() => { pending.delete(id); reject(new Error(method + ' timed out')); }, 30000);
    pending.set(id, {method, resolve, reject, timer});
    child.stdin.write(JSON.stringify({id, method, params}) + '\n');
  });
  try {
    await call('initialize', {clientInfo:{name:'codexfold_windows_smoke',version:'1.0.0'},capabilities:{experimentalApi:true}});
    child.stdin.write(JSON.stringify({method:'initialized',params:{}}) + '\n');
    const resumed = await call('thread/resume', {
      threadId:session.id, path:mounted, cwd:root,
      modelProvider:'openai', approvalPolicy:'never', sandbox:'read-only',
    });
    assert.equal(resumed.thread.id, session.id);
    const label = process.argv[4] === 'native' ? 'native' : managed ? 'managed' : 'mounted';
    console.log('PASS: local Codex resumes the copied ' + label + ' thread');
    const read = await call('thread/read', {threadId:session.id, includeTurns:true});
    assert.equal(read.thread.id, session.id);
    console.log('PASS: local Codex reads copied ' + label + ' history; turns=' + read.thread.turns.length);
    if (deletionCanary) {
      await call('thread/delete', {threadId:session.id});
      await fs.writeFile(path.join(root, 'client-deletion-result.json'), JSON.stringify({id:session.id,deleted:true}, null, 2));
      console.log('PASS: local Codex deletes the compressed test fork');
      return;
    }
    if (full && process.argv[4] !== 'native') {
      const fork = await call('thread/fork', {threadId:session.id,cwd:root,modelProvider:'openai',approvalPolicy:'never',sandbox:'read-only'});
      assert.notEqual(fork.thread.id, session.id);
      console.log('PASS: local Codex forks the mounted thread');
      await call('thread/archive', {threadId:fork.thread.id});
      await call('thread/unarchive', {threadId:fork.thread.id});
      console.log('PASS: local Codex archives and unarchives its new fork');
      if (process.argv[4] === 'keep-fork') {
        await fs.writeFile(path.join(root, 'client-fork.json'), JSON.stringify({id:fork.thread.id,path:fork.thread.path}, null, 2));
      } else {
        await call('thread/delete', {threadId:fork.thread.id});
        console.log('PASS: local Codex deletes its new native fork');
      }
      if (managed) {
        await call('thread/archive', {threadId:session.id});
        await call('thread/unarchive', {threadId:session.id});
        console.log('PASS: local Codex archives and unarchives the compressed managed thread');
      }
    }
  } finally {
    await fs.writeFile(path.join(root, 'client.stderr.log'), stderr);
    child.stdin.end();
    await new Promise(resolve => {
      if (child.exitCode !== null) return resolve();
      const timer = setTimeout(() => { child.kill(); resolve(); }, 5000);
      child.once('exit', () => { clearTimeout(timer); resolve(); });
    });
    lines.close();
  }
}
main().catch(error => { console.error(error.message); process.exitCode = 1; });

const fs = require('node:fs');
const path = require('node:path');
const cp = require('node:child_process');

async function main() {
  const key = process.env['INPUT_SSH-KEY'];
  if (!key) throw new Error('HULK_SDK_GO_READ_KEY is required to download the private Hulk Go SDK');
  const dir = fs.mkdtempSync(path.join(process.env.RUNNER_TEMP, 'hulk-sdk-go-auth-'));
  fs.chmodSync(dir, 0o700);
  fs.appendFileSync(process.env.GITHUB_STATE, `authDir=${dir}\n`);
  fs.writeFileSync(path.join(dir, 'key'), key + '\n', { mode: 0o600 });
  const response = await fetch('https://api.github.com/meta', { headers: { authorization: `Bearer ${process.env['INPUT_GITHUB-TOKEN']}` }, signal: AbortSignal.timeout(15000) });
  if (!response.ok) throw new Error(`GitHub SSH metadata returned ${response.status}`);
  const metadata = await response.json();
  if (!Array.isArray(metadata.ssh_keys) || metadata.ssh_keys.length === 0 ||
      metadata.ssh_keys.some(k => typeof k !== 'string' || !/^(ssh-|ecdsa-)/.test(k) || /[\r\n]/.test(k))) {
    throw new Error('GitHub SSH metadata has no valid host keys');
  }
  fs.writeFileSync(path.join(dir, 'known_hosts'), metadata.ssh_keys.map(k => `github.com ${k}\n`).join(''), { mode: 0o600 });
  const config = path.join(dir, 'gitconfig');
  const configure = (...args) => cp.execFileSync('git', ['config', '--file', config, ...args], { stdio: 'pipe' });
  configure('include.path', path.join(process.env.HOME, '.gitconfig'));
  configure('url.ssh://git@github.com/adapta-school/hulk-sdk-go.insteadOf', 'https://github.com/adapta-school/hulk-sdk-go');
  configure('core.sshCommand', 'ssh -i "$HULK_SDK_GO_AUTH_DIR/key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$HULK_SDK_GO_AUTH_DIR/known_hosts"');
  fs.appendFileSync(process.env.GITHUB_ENV, `HULK_SDK_GO_AUTH_DIR=${dir}\nGIT_CONFIG_GLOBAL=${config}\nGOPRIVATE=github.com/adapta-school/hulk-sdk-go\n`);
}
main().catch(error => { console.error(error.message); process.exitCode = 1; });

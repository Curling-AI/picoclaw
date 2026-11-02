const fs = require('node:fs');
const path = require('node:path');
const dir = process.env.STATE_authDir;
if (dir && path.dirname(dir) === process.env.RUNNER_TEMP && path.basename(dir).startsWith('hulk-sdk-go-auth-')) {
  fs.rmSync(dir, { recursive: true, force: true });
}

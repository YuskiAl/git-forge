// git-forge entrypoint. One process: bind an ephemeral port, register it
// with the trial environment, serve until the suite reports done, exit 0.
// The shop token is read from the environment the trial provides.

import { createApp } from './src/app.ts';
import { Store } from './src/store.ts';

const envUrl = process.env.MAKEFUN_ENV_URL ?? '';
const shopToken = process.env.MAKEFUN_SHOP_TOKEN ?? '';
const store = new Store();
const server = createApp(store, shopToken);

function postJson(url: string, payload: unknown): Promise<unknown> {
  return new Promise((resolve, reject) => {
    const body = JSON.stringify(payload);
    const request = fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Content-Length': String(Buffer.byteLength(body)) },
      body,
    });
    request.then(async (response) => {
      const text = await response.text();
      try {
        resolve(JSON.parse(text));
      } catch {
        resolve(text);
      }
    }).catch(reject);
  });
}

function getJson(url: string): Promise<unknown> {
  return fetch(url).then(async (response) => {
    const text = await response.text();
    try {
      return JSON.parse(text);
    } catch {
      return text;
    }
  });
}

async function main(): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') throw new Error('could not bind a port');
  const port = address.port;

  if (envUrl === '') {
    // Local development: no trial environment to register with; just serve.
    await new Promise<void>((resolve) => {
      server.on('close', () => resolve());
    });
    return;
  }

  await postJson(`${envUrl}/register`, { port });
  for (;;) {
    const done = await getJson(`${envUrl}/done`);
    if (typeof done === 'object' && done !== null && (done as Record<string, unknown>).done === true) break;
    await new Promise((resolve) => setTimeout(resolve, 1000));
  }
  server.close();
  process.exit(0);
}

main().catch((error: unknown) => {
  console.error(error);
  process.exit(1);
});

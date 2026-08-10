import { test, expect } from '@playwright/test';

test('predicts the local paddle while authoritative snapshots are delayed', async ({ page }) => {
  await page.addInitScript(() => {
    class DelayedSocket {
      static OPEN = 1;
      readyState = 0;
      onopen = null;
      onmessage = null;
      onclose = null;
      onerror = null;

      constructor() {
        window.__pongSent = [];
        window.__pongSocket = this;
        setTimeout(() => {
          this.readyState = DelayedSocket.OPEN;
          this.onopen?.();
          setTimeout(() => this.onmessage?.({
            data: JSON.stringify({ type: 'joined', player: 1 }),
          }), 0);
          setTimeout(() => this.onmessage?.({
            data: JSON.stringify({
              type: 'state',
              state: {
                ball: { x: 0.5, y: 0.5, dx: 0.012, dy: 0 },
                p1: { y: 0.5 },
                p2: { y: 0.5 },
                score1: 0,
                score2: 0,
                status: 'playing',
                winner: 0,
                p1_ready: true,
                p2_ready: true,
                p1_input_sequence: 0,
                p2_input_sequence: 0,
              },
            }),
          }), 10);
        }, 0);
      }

      send(value) {
        window.__pongSent.push(JSON.parse(value));
      }

      close() {
        this.readyState = 3;
        this.onclose?.();
      }
    }

    window.__pongSent = [];
    window.WebTransport = undefined;
    window.WebSocket = DelayedSocket;
  });

  await page.goto('/game.html?room=delayed&name=Predictive%20Player');
  await page.waitForFunction(() => {
    const value = document.getElementById('pongCanvas')?.dataset.playerPaddleY;
    return value !== undefined && value !== '';
  });

  const initial = Number(await page.locator('#pongCanvas').getAttribute('data-player-paddle-y'));
  await page.keyboard.down('w');
  await expect.poll(async () => Number(await page.locator('#pongCanvas').getAttribute('data-player-paddle-y')))
    .toBeLessThan(initial);

  const sentInputs = await page.evaluate(() => window.__pongSent
    .filter((message) => message.type !== 'proxy-ready'));
  expect(sentInputs.length).toBeGreaterThan(0);
  expect(sentInputs.some((message) => Number.isInteger(message.sequence) && message.sequence > 0)).toBeTruthy();
  await page.keyboard.up('w');
});

declare global {
  interface Window {
    __pongSent: Array<{ type?: string; sequence?: number }>;
    __pongSocket: unknown;
  }
}

export {};

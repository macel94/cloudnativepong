import { test, expect } from '@playwright/test';

test('reconnects a transiently disconnected player within the room grace period', async ({ page }) => {
  await page.addInitScript(() => {
    class ReconnectingSocket {
      static OPEN = 1;
      static CLOSED = 3;
      static sockets: ReconnectingSocket[] = [];
      readyState = 0;
      onopen: (() => void) | null = null;
      onmessage: ((event: { data: string }) => void) | null = null;
      onclose: (() => void) | null = null;
      onerror: (() => void) | null = null;

      constructor() {
        ReconnectingSocket.sockets.push(this);
        const first = ReconnectingSocket.sockets.length === 1;
        setTimeout(() => {
          this.readyState = ReconnectingSocket.OPEN;
          this.onopen?.();
          setTimeout(() => this.onmessage?.({
            data: JSON.stringify({
              type: 'joined',
              player: 1,
              reconnect_token: '0123456789abcdef0123456789abcdef',
              input_sequence: first ? 0 : 4,
              ...(first ? {} : { reconnected: true }),
            }),
          }), 0);
          if (first) {
            setTimeout(() => {
              this.readyState = ReconnectingSocket.CLOSED;
              this.onclose?.();
            }, 20);
          }
        }, 0);
      }

      send(value: string) {
        (window as unknown as { __pongSent: unknown[] }).__pongSent.push(JSON.parse(value));
      }

      close() {
        this.readyState = ReconnectingSocket.CLOSED;
        this.onclose?.();
      }
    }

    (window as unknown as { __pongSent: unknown[] }).__pongSent = [];
    (window as unknown as { __pongSockets: typeof ReconnectingSocket }).__pongSockets = ReconnectingSocket;
    window.WebTransport = undefined;
    window.WebSocket = ReconnectingSocket as unknown as typeof WebSocket;
  });

  await page.goto('/game.html?room=reconnect&name=Reconnect%20Player');
  await expect.poll(async () => page.evaluate(() => {
    const sockets = (window as unknown as { __pongSockets: { sockets: unknown[] } }).__pongSockets;
    return sockets.sockets.length;
  }), { timeout: 5000 }).toBeGreaterThanOrEqual(2);

  await expect(page.locator('#status')).toHaveText('Reconnected. Resuming game...');
  await expect.poll(async () => page.evaluate(() => document.cookie))
    .toContain('pong_reconnect_reconnect=0123456789abcdef0123456789abcdef');
});

declare global {
  interface Window {
    WebTransport: unknown;
  }
}

export {};

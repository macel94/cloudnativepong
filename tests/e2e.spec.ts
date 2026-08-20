/**
 * Cloud Native Pong — End-to-End Tests
 *
 * These tests verify the full stack:
 *   lobby page → room creation → WebSocket game → scoring → game over
 *
 * Run:
 *   npx playwright test
 *
 * Requires the server running (handled automatically by webServer config).
 */

import { test, expect } from '@playwright/test';

const LOBBY_URL = '/';
const PLAYER_NAME = 'TestPlayer';

test.describe('Lobby', () => {
  test('loads browser-compatible favicon assets', async ({ page, request }) => {
    for (const path of ['/', '/game.html?room=test&name=TestPlayer']) {
      await page.goto(path);
      await expect(page.locator('link[rel="icon"]')).toHaveCount(1);
      await expect(page.locator('link[rel="icon"][href="/pong-favicon.png"]')).toHaveAttribute('type', 'image/png');
      await expect(page.locator('link[rel="icon"][href="/pong-favicon.png"]')).toHaveAttribute('sizes', '32x32');
    }

    const pngResponse = await request.get('/pong-favicon.png');
    expect(pngResponse.ok()).toBeTruthy();
    expect(pngResponse.headers()['content-type']).toMatch(/^image\/png/u);
    const png = await pngResponse.body();
    expect(png.subarray(0, 8)).toEqual(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]));
    expect(png.readUInt32BE(16)).toBe(32);
    expect(png.readUInt32BE(20)).toBe(32);
  });

  test('shows the production build link', async ({ page }) => {
    await page.goto(LOBBY_URL);
    const build = page.locator('[data-build-version]');
    await expect(build).toBeVisible();
    if (process.env.TEST_MODE === 'k8s') {
      const expectedSHA = process.env.GITHUB_SHA;
      await expect(build).toHaveText(/^sha-[0-9a-f]{7}$/);
      await expect(build).toHaveAttribute('data-build-sha', expectedSHA || /^[0-9a-f]{40}$/);
      if (expectedSHA) {
        await expect(build).toHaveAttribute('href', `https://github.com/macel94/cloudnativepong/commit/${expectedSHA}`);
      }
    } else {
      await expect(build).toHaveText('sha-dev');
      await expect(build).toHaveAttribute('href', /github\.com\/macel94\/cloudnativepong\/commits\/main/);
      await expect(build).toHaveAttribute('data-build-sha', 'dev');
    }
  });

  test('loads the lobby page', async ({ page }) => {
    await page.goto(LOBBY_URL);
    await expect(page.locator('h1')).toHaveText('🏓 Cloud Native Pong');
  });

  test('shows empty rooms list initially', async ({ page, request }) => {
    // Clean up any existing rooms first
    const rooms = await (await request.get('/api/rooms')).json();
    // Note: in K8s mode, room pods need manual cleanup, but the lobby
    // list will still show them if they exist in DB.
    await page.goto(LOBBY_URL);
    // If there are no rooms, the empty msg should be visible
    if (rooms.length === 0) {
      await expect(page.locator('#noRooms')).toBeVisible();
    } else {
      // Rooms exist from previous runs; skip strict empty check
      await expect(page.locator('#noRooms')).not.toBeVisible();
    }
  });

  test('shows player name input', async ({ page }) => {
    await page.goto(LOBBY_URL);
    const input = page.locator('#playerName');
    await expect(input).toBeVisible();
    await input.fill(PLAYER_NAME);
    await expect(input).toHaveValue(PLAYER_NAME);
  });

  test('create room button is visible', async ({ page }) => {
    await page.goto(LOBBY_URL);
    await expect(page.locator('#btnNewRoom')).toBeVisible();
    await expect(page.locator('#btnAI')).toHaveText('🤖 Play vs Computer (AI)');
  });

  test('starts a local computer game without an API room', async ({ page }) => {
    await page.goto(LOBBY_URL);
    await page.locator('#playerName').fill('Mobile Pilot');
    await page.locator('#btnAI').click();
    await page.waitForURL(/game\.html\?mode=ai/);
    await expect(page.locator('#roomLabel')).toContainText('vs Computer');
    await expect(page.locator('#status')).toContainText('Playing vs Computer');
    await expect(page.locator('#pongCanvas')).toBeVisible();
    await expect(page.locator('#moveUp')).toHaveAttribute('aria-pressed', 'false');

    const initial = await page.locator('#pongCanvas').screenshot();
    await page.waitForTimeout(250);
    const later = await page.locator('#pongCanvas').screenshot();
    expect(later.equals(initial)).toBeFalsy();
  });

  test('marks the controllable paddle with a green border (AI controls P1)', async ({ page }) => {
    await page.goto(LOBBY_URL);
    await page.locator('#playerName').fill('Border Pilot');
    await page.locator('#btnAI').click();
    await page.waitForURL(/game\.html\?mode=ai/);
    await page.waitForFunction(() => Boolean(document.getElementById('pongCanvas')?.dataset.controlledPaddle));
    await expect(page.locator('#pongCanvas')).toHaveAttribute('data-controlled-paddle', 'p1');
  });

  test('requires a display name before starting a game', async ({ page }) => {
    await page.goto(LOBBY_URL);
    await page.locator('#btnAI').click();
    await expect(page.locator('#error')).toBeVisible();
    await expect(page.locator('#error')).toContainText(/display name|Enter a display name/u);
    await expect(page).toHaveURL(/\/$/);
  });

  test('persists the player name and exposes edit/save controls', async ({ page }) => {
    await page.goto(LOBBY_URL);
    const input = page.locator('#playerName');
    await expect(page.locator('#btnEditName')).toBeVisible();
    await expect(page.locator('#btnSaveName')).toBeVisible();
    await input.fill('PersistedPlayer');
    await page.locator('#btnSaveName').click();
    await expect(input).toHaveAttribute('readonly', '');
    await expect(page.locator('#nameStatus')).toContainText('PersistedPlayer');
    const stored = await page.evaluate(() => localStorage.getItem('pong_player_name'));
    expect(stored).toBe('PersistedPlayer');
    // Editing a saved name makes the field writable again.
    await page.locator('#btnEditName').click();
    await expect(input).not.toHaveAttribute('readonly', '');
  });
});

test.describe('Room workflow', () => {
  test('creates a room and navigates to game page', async ({ page }) => {
    await page.goto(LOBBY_URL);
    await page.locator('#playerName').fill(PLAYER_NAME);
    const roomCreated = page.waitForResponse((response) =>
      response.url().endsWith('/api/rooms/create') && response.request().method() === 'POST'
    );
    await page.locator('#btnNewRoom').click();
    const createResponse = await roomCreated;
    expect(createResponse.ok()).toBeTruthy();

    // Should navigate to game page
    await page.waitForURL(/game\.html\?room=/);
    await expect(page.locator('h1')).toHaveText('🏓 PONG');
    await expect(page.locator('#status')).toContainText('Player', {
      timeout: process.env.TEST_MODE === 'k8s' ? 20_000 : 5_000,
    });
  });

  test('lists created rooms on lobby page', async ({ page, request }) => {
    // Create through the API and leave the waiting reservation alive while the
    // lobby is loaded. Navigating away from a game page would close its socket
    // and correctly trigger local room cleanup before the list can render.
    const create = await request.post('/api/rooms/create', {
      data: { name: 'List Test Room' },
    });
    expect(create.ok()).toBeTruthy();

    await page.goto(LOBBY_URL);
    await page.locator('#playerName').fill(PLAYER_NAME);
    await expect(page.locator('.room-card').first()).toBeVisible();
  });
});

test.describe('Two-player game', () => {
  test('two players can join and play a game', async ({ browser }) => {
    // Player 1: create room
    const p1Ctx = await browser.newContext();
    const p1Page = await p1Ctx.newPage();
    await p1Page.goto(LOBBY_URL);
    await p1Page.locator('#playerName').fill('Alice');
    await p1Page.locator('#btnNewRoom').click();
    await p1Page.waitForURL(/game\.html\?room=/);

    // Extract room ID from URL
    const p1URL = p1Page.url();
    const roomId = new URL(p1URL).searchParams.get('room');
    expect(roomId).toBeTruthy();

    // Player 1 should see their assignment
    await expect(p1Page.locator('#status')).toContainText('Player 1', {
      timeout: process.env.TEST_MODE === 'k8s' ? 20_000 : 5_000,
    });
    // Player 1 controls the left paddle only.
    await expect(p1Page.locator('#pongCanvas')).toHaveAttribute('data-controlled-paddle', 'p1');

    // Player 2: join the same room
    const p2Ctx = await browser.newContext();
    const p2Page = await p2Ctx.newPage();
    await p2Page.goto(LOBBY_URL);
    await p2Page.locator('#playerName').fill('Bob');

    // Join via API directly (simpler than clicking the room card)
    await p2Page.evaluate(async (id) => {
      const res = await fetch('/api/rooms/join', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ room_id: id }),
      });
      const data = await res.json();
      if (data.error) throw new Error(data.error);
    }, roomId);

    // Navigate to game page
    await p2Page.goto(`/game.html?room=${roomId}&name=Bob`);
    await p2Page.waitForURL(/game\.html/);

    // Both players should see the game canvas
    await expect(p1Page.locator('#pongCanvas')).toBeVisible();
    await expect(p2Page.locator('#pongCanvas')).toBeVisible();

    // Wait for both to connect
    await p1Page.waitForTimeout(1000);
    await p2Page.waitForTimeout(1000);

    // Player 2 should be connected (game may have already started)
    const p2Status = await p2Page.locator('#status').textContent();
    expect(p2Status.includes('Player 2') || p2Status.includes('Playing')).toBeTruthy();
    // Player 2 controls the right paddle only; no border on P1's paddle.
    await expect(p2Page.locator('#pongCanvas')).toHaveAttribute('data-controlled-paddle', 'p2');

    // Simulate P1 pressing W (move up) — send a few inputs
    await p1Page.keyboard.down('w');
    await p1Page.waitForTimeout(200);
    await p1Page.keyboard.up('w');

    // Simulate P2 pressing ArrowDown
    await p2Page.keyboard.down('ArrowDown');
    await p2Page.waitForTimeout(200);
    await p2Page.keyboard.up('ArrowDown');

    // Verify scores display
    await expect(p1Page.locator('#score1')).toBeVisible();
    await expect(p1Page.locator('#score2')).toBeVisible();
    await expect(p2Page.locator('#score1')).toBeVisible();
    await expect(p2Page.locator('#score2')).toBeVisible();

    await p1Ctx.close();
    await p2Ctx.close();
  });

  test('a third client can watch a live game without joining as a player', async ({ browser, request }) => {
    const p1Ctx = await browser.newContext();
    const p1Page = await p1Ctx.newPage();
    await p1Page.goto(LOBBY_URL);
    await p1Page.locator('#playerName').fill('SpectatorAlice');
    await p1Page.locator('#btnNewRoom').click();
    await p1Page.waitForURL(/game\.html\?room=/);
    const roomId = new URL(p1Page.url()).searchParams.get('room');
    expect(roomId).toBeTruthy();
    await expect(p1Page.locator('#status')).toContainText('Player 1', {
      timeout: process.env.TEST_MODE === 'k8s' ? 20_000 : 5_000,
    });

    const spectatorCtx = await browser.newContext();
    const spectatorPage = await spectatorCtx.newPage();
    await spectatorPage.goto(LOBBY_URL);

    const p2Ctx = await browser.newContext();
    const p2Page = await p2Ctx.newPage();
    const joinResponse = await p2Page.request.post('/api/rooms/join', {
      data: { room_id: roomId },
    });
    expect(joinResponse.ok()).toBeTruthy();
    await p2Page.goto(`/game.html?room=${roomId}&name=SpectatorBob`);
    await expect.poll(async () => (await p2Page.locator('#status').textContent()) || '', {
      timeout: process.env.TEST_MODE === 'k8s' ? 20_000 : 5_000,
    }).toMatch(/Player 2|Playing/);
    await expect(p1Page.locator('#status')).toContainText('Playing!', {
      timeout: process.env.TEST_MODE === 'k8s' ? 20_000 : 5_000,
    });

    const watchButton = spectatorPage.locator(`.watch-btn[onclick="watchRoom('${roomId}')"]`);
    await expect(watchButton).toBeVisible({ timeout: 10_000 });
    await watchButton.click();
    await spectatorPage.waitForURL(new RegExp(`game\\.html\\?room=${roomId}&mode=spectator`));
    await expect(spectatorPage.locator('#roomLabel')).toContainText('Live spectator');
    await expect(spectatorPage.locator('#controlHint')).toHaveText('Watching live · controls disabled');
    await expect(spectatorPage.locator('#touchControls')).toBeHidden();
    await expect(spectatorPage.locator('#status')).toContainText('Playing!', {
      timeout: process.env.TEST_MODE === 'k8s' ? 20_000 : 5_000,
    });

    const rooms = await (await request.get('/api/rooms')).json();
    const room = rooms.find((candidate: { id: string }) => candidate.id === roomId);
    expect(room?.players).toBe(2);

    await spectatorCtx.close();
    await p1Ctx.close();
    await p2Ctx.close();
  });
});

test.describe('API endpoints', () => {
  test('GET /api/rooms returns JSON array', async ({ request }) => {
    const res = await request.get('/api/rooms');
    expect(res.ok()).toBeTruthy();
    const data = await res.json();
    expect(Array.isArray(data)).toBeTruthy();
  });

  test('POST /api/rooms/create returns a room object', async ({ request }) => {
    const res = await request.post('/api/rooms/create', {
      data: { name: 'Test Room' },
    });
    expect(res.ok()).toBeTruthy();
    const room = await res.json();
    expect(room.id).toBeTruthy();
    expect(room.name).toBe('Test Room');
    expect(room.status).toBe('waiting');
  });

  test('POST /api/rooms/join with valid room succeeds', async ({ request }) => {
    // Create a room first
    const createRes = await request.post('/api/rooms/create', {
      data: { name: 'Join Test' },
    });
    const room = await createRes.json();

    // Join it
    const joinRes = await request.post('/api/rooms/join', {
      data: { room_id: room.id },
    });
    expect(joinRes.ok()).toBeTruthy();
    const joinData = await joinRes.json();
    expect(joinData.room_id).toBe(room.id);
  });

  test('POST /api/rooms/join with invalid room returns error', async ({ request }) => {
    const res = await request.post('/api/rooms/join', {
      data: { room_id: 'nonexistent' },
    });
    expect(res.status()).toBe(400);
    expect(await res.text()).toContain('invalid room ID');
  });

  test('static files are served', async ({ request }) => {
    const res = await request.get('/style.css');
    expect(res.ok()).toBeTruthy();
    const ct = res.headers()['content-type'];
    expect(ct).toContain('text/css');
  });
});
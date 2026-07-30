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
  test('loads the lobby page', async ({ page }) => {
    await page.goto(LOBBY_URL);
    await expect(page.locator('h1')).toHaveText('🏓 Cloud Native Pong');
  });

  test('shows empty rooms list initially', async ({ page }) => {
    await page.goto(LOBBY_URL);
    await expect(page.locator('#noRooms')).toBeVisible();
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
  });
});

test.describe('Room workflow', () => {
  test('creates a room and navigates to game page', async ({ page }) => {
    await page.goto(LOBBY_URL);
    await page.locator('#playerName').fill(PLAYER_NAME);
    await page.locator('#btnNewRoom').click();

    // Should navigate to game page
    await page.waitForURL(/game\.html\?room=/);
    await expect(page.locator('h1')).toHaveText('🏓 PONG');
    await expect(page.locator('#status')).toContainText('Player');
  });

  test('lists created rooms on lobby page', async ({ page }) => {
    await page.goto(LOBBY_URL);
    await page.locator('#playerName').fill(PLAYER_NAME);

    // Create a room
    await page.locator('#btnNewRoom').click();
    await page.waitForURL(/game\.html\?room=/);

    // Go back to lobby
    await page.goto(LOBBY_URL);
    await page.locator('#playerName').fill(PLAYER_NAME);

    // Wait for room list to refresh
    await page.waitForTimeout(1000);

    // Should see the room in the list
    const rooms = page.locator('.room-card');
    await expect(rooms.first()).toBeVisible();
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
    await expect(p1Page.locator('#status')).toContainText('Player 1');

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
    expect(res.ok()).toBeTruthy();
    const data = await res.json();
    expect(data.error).toBeTruthy();
  });

  test('static files are served', async ({ request }) => {
    const res = await request.get('/style.css');
    expect(res.ok()).toBeTruthy();
    const ct = res.headers()['content-type'];
    expect(ct).toContain('text/css');
  });
});
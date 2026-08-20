import { test, expect } from '@playwright/test';

// The game and lobby always leave at least one realpath row in the browser
// console (no opt-in needed), so a normal session can be inspected for lag
// without turning anything on. These tests only assert that the rows appear.
function collectConsole(page) {
  const lines = [];
  page.on('console', (msg) => {
    try {
      const t = String(msg.text());
      if (t) lines.push(t);
    } catch {
      /* ignore malformed console events */
    }
  });
  return lines;
}

test('game page prints a default compact diag row (AI baseline)', async ({ page }) => {
  const lines = collectConsole(page);
  await page.goto('/game.html?mode=ai&name=Probe');
  await page.waitForTimeout(300);
  await page.keyboard.press('KeyW');
  await page.waitForTimeout(2000);
  test.info().attach('console', Buffer.from(lines.join('\n')));
  const rows = lines.filter((l) => l.includes('stateHz'));
  expect(rows.length).toBeGreaterThan(0); // the 'ai' baseline row
});

test('?diag=1 lobby prints an ON hint and goes full-report', async ({ page }) => {
  const lines = collectConsole(page);
  await page.goto('/?diag=1');
  await expect(page).toHaveURL(/\?diag=1/);
  await page.waitForTimeout(400);
  const hint = lines.find((l) => l.includes('diagnostics ON'));
  expect(hint).toBeTruthy();
});
import { test, expect } from '@playwright/test';

test('supports a narrow touch viewport and touch paddle controls', async ({ page }) => {
  await page.goto('/game.html?mode=ai&name=Touch%20Player');
  await expect(page.locator('#pongCanvas')).toBeVisible();
  await expect(page.locator('#pongCanvas')).toHaveCSS('width', /px/);
  await expect(page.locator('#moveUp')).toBeVisible();
  await expect(page.locator('#moveDown')).toBeVisible();

  const canvasBox = await page.locator('#pongCanvas').boundingBox();
  expect(canvasBox).not.toBeNull();
  expect(canvasBox.width).toBeLessThanOrEqual(390);
  expect(canvasBox.width).toBeGreaterThan(250);

  await page.waitForFunction(() => Boolean(document.getElementById('pongCanvas')?.dataset.playerPaddleY));
  const initialPaddleY = Number(await page.locator('#pongCanvas').getAttribute('data-player-paddle-y'));
  await page.locator('#moveUp').dispatchEvent('pointerdown', {
    pointerId: 1,
    pointerType: 'touch',
    isPrimary: true,
  });
  await expect(page.locator('#moveUp')).toHaveAttribute('aria-pressed', 'true');
  await expect.poll(async () => Number(await page.locator('#pongCanvas').getAttribute('data-player-paddle-y')))
    .toBeLessThan(initialPaddleY);
  await page.locator('#moveUp').dispatchEvent('pointerup', {
    pointerId: 1,
    pointerType: 'touch',
    isPrimary: true,
  });
  await expect(page.locator('#moveUp')).toHaveAttribute('aria-pressed', 'false');
});

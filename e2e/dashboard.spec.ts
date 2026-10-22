// The dashboard's own e2e specs. They assume a live serve on
// DEV_LEASE_E2E_URL (default http://127.0.0.1:8410) with two demo stacks:
// devlease-it-demo-a (a kept service and a web service, one volume) and
// devlease-it-demo-b (one web service), where demo-b has already been
// stopped by the setup. The setup lives in the CI workflow and in
// e2e/README.md.
//
// One worker and no parallelism on purpose: the specs share one serve and
// the flows change the demo stacks' state. Declaration order is the run
// order, so the state-independent matrix runs before the flows.
import path from 'node:path';
import { test, expect, type Page } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';

const DEMO_A = 'devlease-it-demo-a';
const DEMO_B = 'devlease-it-demo-b';

const VIEWPORTS = [
  { key: 'desktop', width: 1440, height: 900 },
  { key: 'phone', width: 390, height: 844 },
] as const;

const THEMES = ['light', 'dark'] as const;

// Screenshots land in DEV_LEASE_E2E_SHOTS (default e2e/shots/ relative to
// the e2e directory the specs run from), gitignored either way.
const shotsDir = process.env.DEV_LEASE_E2E_SHOTS ?? path.join(__dirname, 'shots');

function shot(name: string): string {
  return path.join(shotsDir, name);
}

async function expectNoHorizontalScroll(page: Page): Promise<void> {
  const widths = await page.evaluate(() => ({
    scroll: document.documentElement.scrollWidth,
    client: document.documentElement.clientWidth,
  }));
  expect(widths.scroll, `the page scrolls horizontally (${widths.scroll} > ${widths.client})`).toBeLessThanOrEqual(
    widths.client,
  );
}

async function expectNoSeriousViolations(page: Page): Promise<void> {
  const results = await new AxeBuilder({ page }).analyze();
  const serious = results.violations.filter((v) => v.impact === 'serious' || v.impact === 'critical');
  expect(serious, `axe violations:\n${JSON.stringify(serious, null, 2)}`).toEqual([]);
}

// Both pages, every viewport, every theme: renders, has no horizontal
// scroll, passes axe and leaves a screenshot. The pages here only depend
// on the initial state (both stacks live), so this matrix runs before the
// flows below change anything.
for (const vp of VIEWPORTS) {
  for (const theme of THEMES) {
    test.describe(`${theme} theme at ${vp.width}x${vp.height}`, () => {
      test.use({ viewport: { width: vp.width, height: vp.height }, colorScheme: theme });

      test('the all-stacks page renders, fits and passes axe', async ({ page }) => {
        await page.goto('/');
        await expect(page.locator('.stacks')).toBeVisible();
        // The phone layout swaps the table for cards; either way each
        // stack has exactly one visible row.
        await expect(page.locator(`[data-stack="${DEMO_A}"]`).filter({ visible: true })).toHaveCount(1);
        await expect(page.locator(`[data-stack="${DEMO_B}"]`).filter({ visible: true })).toHaveCount(1);
        await expectNoHorizontalScroll(page);
        await expectNoSeriousViolations(page);
        await page.screenshot({ path: shot(`stacks-${vp.key}-${theme}.png`), fullPage: true });
      });

      test('the stack page renders, fits and passes axe', async ({ page }) => {
        await page.goto(`/stacks/${DEMO_A}`);
        await expect(page.locator('h1.stack-title')).toHaveText(DEMO_A);
        await expectNoHorizontalScroll(page);
        await expectNoSeriousViolations(page);
        await page.screenshot({ path: shot(`stack-${vp.key}-${theme}.png`), fullPage: true });
      });
    });
  }
}

test.describe('actions', () => {
  test.use({ viewport: { width: 1440, height: 900 }, colorScheme: 'light' });

  test('the stop dialog stops the live stack while the kept service keeps running', async ({ page }) => {
    await page.goto(`/stacks/${DEMO_A}`);
    await expect(page.locator('h1.stack-title')).toHaveText(DEMO_A);

    // Opening the dialog, then cancelling it, closes it again.
    await page.locator('[data-dialog="stop-dialog"]').click();
    const dialog = page.locator('#stop-dialog');
    await expect(dialog).toBeVisible();
    await dialog.locator('.dialog-cancel').click();
    await expect(dialog).not.toBeVisible();

    // The confirmed stop stops the stack and keeps the kept service.
    await page.locator('[data-dialog="stop-dialog"]').click();
    await expect(dialog).toBeVisible();
    await page.screenshot({ path: shot('stop-dialog-desktop-light.png') });
    await dialog.locator('button[type="submit"]').click();

    await expect(page.locator('.flash')).toContainText(`Stopped ${DEMO_A}.`);
    await expect(page.locator('span.badge').first()).toHaveText('stopped');

    // The kept service keeps running, the other one exited.
    const rows = page.locator('.containers-table tbody tr');
    await expect(rows).toHaveCount(2);
    await expect(rows.nth(0).locator('td').nth(0)).toContainText('kept');
    await expect(rows.nth(0).locator('td').nth(1)).toHaveText('running');
    await expect(rows.nth(1).locator('td').nth(0)).toContainText('web');
    await expect(rows.nth(1).locator('td').nth(1)).toHaveText('exited');
  });

  test('the remove dialog needs the typed name and removes the stopped stack', async ({ page }) => {
    // The setup stopped demo-b: the page says so.
    await page.goto(`/stacks/${DEMO_B}`);
    await expect(page.locator('span.badge').first()).toHaveText('stopped');

    await page.locator('[data-dialog="remove-dialog"]').click();
    const dialog = page.locator('#remove-dialog');
    await expect(dialog).toBeVisible();

    // The button only works once the typed name matches the stack name.
    const button = dialog.locator('button[type="submit"]');
    await expect(button).toBeDisabled();
    const input = dialog.locator('input[name="confirm"]');
    await input.fill(DEMO_B.slice(0, -1));
    await expect(button).toBeDisabled();
    await page.screenshot({ path: shot('remove-dialog-desktop-light.png') });
    await input.fill(DEMO_B);
    await expect(button).toBeEnabled();
    await button.click();

    // The removal lands on the all-stacks page: the flash says so and the
    // stack is gone from the listing.
    await expect(page.locator('.flash')).toContainText(`Removed ${DEMO_B}.`);
    await expect(page.locator(`[data-stack="${DEMO_B}"]`)).toHaveCount(0);
  });

  test('the 15 s refresh keeps polling after two ticks', async ({ page }) => {
    const fragmentRequests: string[] = [];
    page.on('request', (req) => {
      if (req.url().includes('/fragments/stacks')) {
        fragmentRequests.push(req.url());
      }
    });

    await page.goto('/');
    await expect(page.locator('.stacks')).toBeVisible();

    // Two ticks of the 15 s refresh: the first lands near 15 s, the second
    // near 30 s.
    await expect.poll(() => fragmentRequests.length, { timeout: 45_000 }).toBeGreaterThanOrEqual(2);
  });

  test('the refresh swaps sections and never touches the Keep select', async ({ page }) => {
    const fragmentRequests: string[] = [];
    page.on('request', (req) => {
      if (req.url().includes(`/fragments/stacks/${DEMO_A}`)) {
        fragmentRequests.push(req.url());
      }
    });

    await page.goto(`/stacks/${DEMO_A}`);
    await expect(page.locator('[data-refresh-url]')).toBeVisible();

    await page.selectOption('select[name="ttl"]', '8h');
    await expect(page.locator('select[name="ttl"]')).toHaveValue('8h');

    // The 15 s refresh replaces the read-only sections, not the form.
    // Mark the sections first: only a real swap removes the marks.
    await page.evaluate(() => {
      document.querySelectorAll('[id^="frag-"]').forEach((el) => {
        (el as HTMLElement).dataset.old = '1';
      });
    });
    await expect(page.locator('[id^="frag-"][data-old]')).toHaveCount(4);
    const tick = page.waitForResponse((r) => r.url().includes(`/fragments/stacks/${DEMO_A}`), { timeout: 45_000 });
    await tick;
    await expect(page.locator('[id^="frag-"][data-old]')).toHaveCount(0);
    await expect(page.locator('[id^="frag-"]')).toHaveCount(4);
    // After the swap the chosen 8h is still selected.
    await expect(page.locator('select[name="ttl"]')).toHaveValue('8h');
    expect(fragmentRequests.length).toBeGreaterThanOrEqual(1);
  });
});

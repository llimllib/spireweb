// Browser-side behaviour, which nothing else in the suite can reach.
//
// markup_test.go pins the selectors app.ts depends on and says in its own
// comment that it cannot tell whether j moves the cursor. These can. They are
// deliberately a smoke suite: one assertion per behaviour that would be
// invisible from Go, not a second copy of the handler tests.
import { expect, test } from "@playwright/test";

test("opens on the newest session", async ({ page }) => {
  await page.goto("/");

  // Sessions are newest first, and the newest fixture is charlie.
  const rows = page.locator("a.row");
  await expect(rows).toHaveCount(3);
  await expect(rows.first()).toHaveAttribute("href", "/sessions/e2e-charlie");
  await expect(rows.first()).toHaveAttribute("aria-current", "page");

  // And its transcript is what the reading pane shows.
  await expect(page.locator(".reading-head h1")).toContainText(
    "walk me through the ingest pipeline",
  );
  await expect(page.locator(".reading-id")).toHaveText("e2e-charlie");
});

test("typing filters the list", async ({ page }) => {
  await page.goto("/");
  await page.fill('input[name="q"]', "flexbox");

  // htmx debounces, and toHaveCount retries until it settles.
  const rows = page.locator("a.row");
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toHaveAttribute("href", /e2e-alpha/);

  // The excerpt says why it matched, with the term marked.
  await expect(rows.first().locator("mark")).toHaveText(/flexbox/i);
});

test("a tool call expands and fetches its output", async ({ page }) => {
  await page.goto("/sessions/e2e-bravo");

  const tool = page.locator("details.tool");
  await expect(tool).toHaveCount(1);

  // Tool bodies are ~98% of a session's bytes, so they are not in the page
  // until asked for.
  await expect(page.locator(".tool-output")).toHaveCount(0);

  await tool.locator("summary").click();
  await expect(page.locator(".tool-output")).toContainText(
    "tighten the retry budget",
  );
});

test("j and k move the cursor, and the cursor is focus", async ({ page }) => {
  await page.goto("/");
  const rows = page.locator("a.row");

  // From the selected row, which is the first one.
  await page.keyboard.press("j");
  await expect(rows.nth(1)).toBeFocused();

  await page.keyboard.press("j");
  await expect(rows.nth(2)).toBeFocused();

  await page.keyboard.press("k");
  await expect(rows.nth(1)).toBeFocused();

  // G and gg are the ends of the list.
  await page.keyboard.press("Shift+G");
  await expect(rows.last()).toBeFocused();
  await page.keyboard.press("g");
  await page.keyboard.press("g");
  await expect(rows.first()).toBeFocused();

  // Typing must not move it: j is a letter first and a binding second.
  await page.fill('input[name="q"]', "j");
  await expect(page.locator('input[name="q"]')).toBeFocused();
});

test("a search result scrolls to its match", async ({ page }) => {
  await page.goto("/sessions/e2e-charlie?q=quicksand");

  // The term appears once, in the last message of a 41-message session, so it
  // starts well below the fold.
  const match = page.locator(".turn.is-match");
  await expect(match).toHaveCount(1);
  await expect(match).toContainText("quicksand");
  await expect(match).toBeInViewport();

  // Which means the pane actually moved.
  const scrolled = await page
    .locator(".reading")
    .evaluate((pane) => pane.scrollTop);
  expect(scrolled).toBeGreaterThan(0);
});

test("browsing scrolls nowhere", async ({ page }) => {
  await page.goto("/sessions/e2e-charlie");

  await expect(page.locator(".reading")).not.toHaveAttribute("data-scroll-to");
  await expect(page.locator(".turn.is-match")).toHaveCount(0);
  const scrolled = await page
    .locator(".reading")
    .evaluate((pane) => pane.scrollTop);
  expect(scrolled).toBe(0);
});

test("copy as markdown copies the source, not the rendering", async ({
  page,
  context,
}) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto("/sessions/e2e-alpha");

  const turn = page.locator(".turn-assistant");
  const copy = turn.locator("button.turn-copy");

  // Out of the way until the turn is hovered.
  await expect(turn.locator(".turn-tools")).toHaveCSS("opacity", "0");
  await turn.hover();
  await expect(turn.locator(".turn-tools")).toHaveCSS("opacity", "1");

  await copy.click();
  await expect(copy).toHaveAttribute("aria-label", "Copied");

  // Backticks and asterisks survive, which a selection of the rendered prose
  // would have lost.
  const copied = await page.evaluate(() => navigator.clipboard.readText());
  expect(copied).toBe(
    "Use flexbox: `display: flex`, then **`place-items: center`**.",
  );
});

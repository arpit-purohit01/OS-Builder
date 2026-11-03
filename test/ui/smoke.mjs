// UI smoke test: sign in, create org + project, walk the builder, trigger a
// catalog conflict and fix it with one click, undo/redo, save a version.
// Usage: node test/ui/smoke.mjs [baseURL] [outDir]
import { createRequire } from "node:module";
const { chromium } = createRequire(import.meta.url)("playwright"); // resolved via NODE_PATH

const base = process.argv[2] ?? "http://127.0.0.1:8080";
const out = process.argv[3] ?? "out/ui";
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1360, height: 900 } });
const errors = [];
page.on("pageerror", (e) => errors.push(e.message));
// Expected: 401 from /v1/me before sign-in, 409 when the org slug is taken.
page.on("response", (r) => r.status() >= 500 && errors.push(`${r.status()} ${r.url()}`));
const shot = (n) => page.screenshot({ path: `${out}/${n}.png`, fullPage: true });
const step = (s) => console.log("•", s);

await page.goto(base);
step("login");
await page.getByLabel("Work email").fill(`ui-${Date.now()}@example.com`);
await page.getByRole("button", { name: "Email me a code" }).click();
const code = await page.locator("code").first().textContent();
await page.getByLabel(/6-digit code/).fill(code);
await shot("01-login");
await page.getByRole("button", { name: "Sign in" }).click();

step("create org");
await page.getByLabel("Name").fill(`UI Test Org`);
await page.getByRole("button", { name: "Create" }).click();
await page.getByRole("heading", { name: "Projects" }).waitFor();
await shot("02-org-home");

step("new project");
await page.getByRole("button", { name: "New OS" }).first().click();
await page.getByLabel("Name").fill("Aurora Desk");
await page.getByRole("button", { name: /School computer lab/ }).click();
await page.getByRole("button", { name: "Create and open builder" }).click();
await page.getByRole("heading", { name: "Foundation" }).waitFor();
await page.getByText("All choices work together").waitFor({ timeout: 15000 });
await shot("03-builder-foundation");

step("look & feel");
await page.getByRole("link", { name: /Look & feel/ }).click();
await page.getByRole("button", { name: "#DC2626" }).click();
await page.getByRole("button", { name: "Dark" }).click();
await page.getByRole("radio", { name: /Dusk/ }).click();
await shot("04-look");

step("conflict: Flatseal needs developer access; pick it under Expert");
await page.getByRole("button", { name: "Expert", exact: true }).click();
await page.getByRole("link", { name: /^Apps/ }).click();
await page.getByRole("checkbox", { name: /Flatseal/ }).click();
await page.getByText("These choices cannot be combined").first().waitFor({ timeout: 10000 });
const ms = await page.getByText(/checked in/).textContent();
await shot("05-conflict");
step("one-click fix");
await page.locator(".aside .issue button").first().click();
await page.getByText("All choices work together").waitFor({ timeout: 10000 });

step("undo/redo");
await page.getByRole("button", { name: "Undo" }).click();
await page.getByText("These choices cannot be combined").first().waitFor({ timeout: 10000 });
await page.getByRole("button", { name: "Redo" }).click();
await page.getByText("All choices work together").waitFor({ timeout: 10000 });

step("dangerous choice asks for confirmation");
await page.getByRole("link", { name: /Security & access/ }).click();
await page.getByRole("radio", { name: /Secure Boot off/ }).click();
await page.getByRole("heading", { name: "Turn off Secure Boot?" }).waitFor();
await shot("06-confirm");
await page.getByRole("button", { name: "Cancel" }).click();

step("review & save");
await page.getByRole("link", { name: "Review & build" }).first().click();
await page.getByRole("button", { name: "Spec" }).click();
await shot("07-review");
await page.getByRole("button", { name: "Save version" }).click();
await page.getByText(/Saved a new version|No changes/).waitFor({ timeout: 15000 });
await page.getByRole("button", { name: "Changes" }).click();

step("project page shows version");
await page.getByRole("link", { name: "Aurora Desk" }).click();
await page.getByRole("heading", { name: "Versions" }).waitFor();
await page.locator("td", { hasText: "1.0.0" }).first().waitFor();
await shot("08-project");

await browser.close();
console.log("validation round trip:", ms);
if (errors.length) {
  console.error("browser errors:\n" + errors.join("\n"));
  process.exit(1);
}
console.log("UI smoke OK");

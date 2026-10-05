import { test, expect, request } from "@playwright/test";
const origin = new URL(
  process.env.SWITCHYARD_WEB_URL ?? "http://localhost:3000",
).origin;

test("a different admin reviews, approves and applies a production proposal", async ({
  page,
}) => {
  test.setTimeout(90000);
  const password = process.env.SWITCHYARD_DEMO_PASSWORD;
  if (!password)
    throw new Error("Set SWITCHYARD_DEMO_PASSWORD and seed the API first.");
  await page.goto("/");
  await page.getByLabel("Email", { exact: true }).fill("reviewer@example.test");
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Projects & environments" }),
  ).toBeVisible();
  const name = `Review browser ${Date.now()}`;
  await page.getByLabel("Project name", { exact: true }).fill(name);
  await page
    .getByRole("button", { name: "Create project", exact: true })
    .click();
  await expect(
    page
      .getByRole("combobox", { name: "Project", exact: true })
      .locator("option:checked"),
  ).toHaveText(name);
  const projectID = await page
    .getByRole("combobox", { name: "Project", exact: true })
    .inputValue();
  const session = await (
    await page.request.get("/api/backend/v1/session")
  ).json();
  const csrf = session.csrf_token as string;
  expect(
    (
      await page.request.post(`/api/backend/v1/projects/${projectID}/members`, {
        headers: { origin, "x-csrf-token": csrf },
        data: { user_id: "demo_developer" },
      })
    ).status(),
  ).toBe(204);
  const environments = await (
    await page.request.get(`/api/backend/v1/projects/${projectID}/environments`)
  ).json();
  const production = environments.find(
    (e: { name: string }) => e.name === "production",
  ).id as string;

  // The developer proposes through the same public API; the reviewer never sees a direct write.
  const developer = await request.newContext({ baseURL: origin });
  const login = await developer.post("/api/backend/v1/session", {
    headers: { origin },
    data: { email: "developer@example.test", password },
  });
  expect(login.status()).toBe(200);
  const developerCSRF = (await login.json()).csrf_token as string;
  const off = { type: "boolean", data: false };
  const proposed = await developer.post(
    `/api/backend/v1/projects/${projectID}/proposals`,
    {
      headers: { origin, "x-csrf-token": developerCSRF },
      data: {
        environment_id: production,
        key: "listing_flow",
        kind: "create",
        type: "boolean",
        expected_revision: 0,
        default: off,
        safe: off,
        rules: [],
        rollout: { traffic_bp: 500, value: { type: "boolean", data: true } },
        rationale: "Launch the simplified listing flow at 5%",
      },
    },
  );
  expect(proposed.status()).toBe(201);

  await page
    .getByRole("combobox", { name: "Environment", exact: true })
    .selectOption({ label: "production" });
  await page.getByRole("button", { name: "Reviews", exact: true }).click();
  const card = page.getByRole("article", { name: "Proposal listing_flow" });
  await expect(card).toBeVisible();
  await expect(card.getByText("validated")).toBeVisible();
  await expect(card.getByText("rollout 5.00% → true")).toBeVisible();
  // A reason is mandatory and is recorded in the audit history.
  await card.getByRole("button", { name: "Approve this exact diff" }).click();
  await expect(page.getByText("Enter a reason first")).toBeVisible();
  await page
    .getByLabel("Review reason", { exact: true })
    .fill("Diff matches the launch plan");
  await card.getByRole("button", { name: "Approve this exact diff" }).click();
  await expect(card.getByText("approved by demo_reviewer")).toBeVisible();
  await card.getByRole("button", { name: "Apply", exact: true }).click();
  await expect(card.getByText("applied at revision 1")).toBeVisible();
  await expect(card.getByRole("button", { name: "Apply" })).toHaveCount(0);

  await page.getByRole("button", { name: "Audit", exact: true }).click();
  await expect(page.getByText("proposal.applied").first()).toBeVisible();
  await expect(page.getByText("proposal.approved").first()).toBeVisible();
});

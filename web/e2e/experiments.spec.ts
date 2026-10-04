import { test, expect } from "@playwright/test";
const origin = new URL(
  process.env.SWITCHYARD_WEB_URL ?? "http://localhost:3000",
).origin;
test("reviewer creates an unequal A/B run, controls lifecycle and inspects empty cohorts", async ({
  page,
}) => {
  test.setTimeout(60000);
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
  const name = `Experiment browser ${Date.now()}`;
  await page.getByLabel("Project name", { exact: true }).fill(name);
  await page
    .getByRole("button", { name: "Create project", exact: true })
    .click();
  await expect(
    page
      .getByRole("combobox", { name: "Project", exact: true })
      .locator("option:checked"),
  ).toHaveText(name);
  await page.getByRole("button", { name: "Create flag", exact: true }).click();
  await page.getByLabel("Flag key", { exact: true }).fill("listing_flow");
  await page
    .getByLabel("Change reason", { exact: true })
    .fill("Baseline for listing experiment");
  await page.getByRole("button", { name: "Save flag", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Evaluation preview" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Experiments", exact: true }).click();
  await page
    .getByRole("button", { name: "Create experiment", exact: true })
    .click();
  await page
    .getByLabel("Experiment name", { exact: true })
    .fill("Listing completion A/B");
  await page.getByLabel("Eligible traffic (%)", { exact: true }).fill("80");
  await page.getByLabel("Control allocation (%)", { exact: true }).fill("30");
  await page
    .getByLabel("Experiment reason", { exact: true })
    .fill("Simplifying listing should improve completion");
  await page.getByRole("button", { name: "Create draft", exact: true }).click();
  const runs = page.getByRole("table", { name: "Experiment runs" });
  await expect(
    runs.getByRole("cell", { name: "draft", exact: true }),
  ).toBeVisible();
  await expect(
    runs.getByRole("cell", { name: "80.00%", exact: true }),
  ).toBeVisible();
  const results = page.getByRole("table", {
    name: "Variant conversion results",
  });
  await expect(results.locator("tbody tr")).toHaveCount(2);
  for (const variant of ["control", "treatment"]) {
    const row = results.locator("tbody tr").filter({ hasText: variant });
    await expect(row.locator("td").nth(2)).toHaveText("0");
    await expect(row.locator("td").nth(3)).toHaveText("0");
    await expect(row.locator("td").nth(4)).toHaveText("—");
  }
  await page
    .getByLabel("Lifecycle reason", { exact: true })
    .fill("Begin the development trial");
  await page
    .getByRole("button", { name: "Start experiment", exact: true })
    .click();
  await expect(
    runs.getByRole("cell", { name: "running", exact: true }),
  ).toBeVisible();
  const projectID = await page
    .getByRole("combobox", { name: "Project", exact: true })
    .inputValue();
  const environmentID = await page
    .getByRole("combobox", { name: "Environment", exact: true })
    .inputValue();
  const runList = await (
    await page.request.get(
      `/api/backend/v1/projects/${projectID}/experiments?environment_id=${environmentID}`,
    )
  ).json();
  const runID = runList[0].id;
  await page.getByRole("button", { name: "Listing demo", exact: true }).click();
  const keyResponse = page.waitForResponse(
    (response) =>
      response.url().endsWith(`/projects/${projectID}/application-keys`) &&
      response.request().method() === "POST",
  );
  await page
    .getByRole("button", { name: "Enable listing demo", exact: true })
    .click();
  const application = await (await keyResponse).json();
  await expect(
    page.getByRole("button", { name: "Render assigned flow", exact: true }),
  ).toBeVisible();
  let lostAcknowledgement = false;
  await page.route("**/api/backend/v1/events", async (route) => {
    const body = route.request().postDataJSON();
    if (
      !lostAcknowledgement &&
      body.events.some(
        (event: { kind: string }) => event.kind === "listing_completion",
      )
    ) {
      const response = await route.fetch();
      expect(response.status()).toBe(200);
      lostAcknowledgement = true;
      await route.abort("failed");
    } else await route.continue();
  });
  const exposed: Record<string, number> = { control: 0, treatment: 0 };
  const completed = new Set<string>();
  for (let candidate = 0; candidate < 100 && completed.size < 2; candidate++) {
    await page
      .getByLabel("Demo user ID", { exact: true })
      .fill(`browser_demo_${candidate}`);
    const evaluation = page.waitForResponse((response) =>
      response.url().endsWith("/v1/evaluate"),
    );
    await page
      .getByRole("button", { name: "Render assigned flow", exact: true })
      .click();
    const decision = await (await evaluation).json();
    if (decision.reason !== "experiment") {
      await expect(
        page.getByText("No exposure was recorded.", { exact: false }),
      ).toBeVisible();
      continue;
    }
    await expect(
      page.getByText("Exposure accepted after rendering.", { exact: true }),
    ).toBeVisible();
    exposed[decision.variant_id]++;
    if (completed.has(decision.variant_id)) continue;
    if (decision.value.data === false)
      await page
        .getByRole("button", { name: "Continue listing", exact: true })
        .click();
    await page
      .getByRole("button", { name: "Submit demo listing", exact: true })
      .click();
    if (completed.size === 0) {
      await expect(
        page.getByText(
          "Submission succeeded; measurement acknowledgement is pending.",
          { exact: true },
        ),
      ).toBeVisible();
      const replay = page.waitForResponse((response) =>
        response.url().endsWith("/v1/events"),
      );
      await page
        .getByRole("button", {
          name: "Retry measurement delivery",
          exact: true,
        })
        .click();
      const receipts = (await (await replay).json()).receipts;
      expect(
        receipts.every((receipt: { duplicate: boolean }) => receipt.duplicate),
      ).toBe(true);
    }
    await expect(
      page.getByText(
        "Completion and request outcome accepted. Open Experiments to inspect the counts.",
        { exact: true },
      ),
    ).toBeVisible();
    completed.add(decision.variant_id);
  }
  expect(completed.size).toBe(2);
  expect(lostAcknowledgement).toBe(true);
  await page.getByRole("button", { name: "Experiments", exact: true }).click();
  for (const variant of ["control", "treatment"]) {
    const row = results.locator("tbody tr").filter({ hasText: variant });
    await expect(row.locator("td").nth(2)).toHaveText(String(exposed[variant]));
    await expect(row.locator("td").nth(3)).toHaveText("1");
  }
  await page.screenshot({
    path: "../.cache/dashboard-measured-results.png",
    fullPage: true,
  });
  const measured = await (
    await page.request.get(
      `/api/backend/v1/projects/${projectID}/experiments/${runID}/results`,
    )
  ).json();
  for (const variant of measured.variants) {
    expect(variant.provisional.converted).toBe(1);
    expect(variant.finalized.exposed).toBe(0);
    expect(variant.requests.count).toBe(1);
    expect(variant.requests.error_rate).toBe(0);
    expect(variant.requests.p95_upper_bound_ms).not.toBeNull();
  }
  expect(
    (
      await page.request.post("/api/backend/v1/evaluate", {
        headers: {
          Origin: origin,
          Authorization: `Bearer ${application.token}`,
        },
        data: {
          project_id: projectID,
          environment_id: environmentID,
          key: "listing_flow",
          user_id: "after_revoke",
          fallback: { type: "boolean", data: false },
        },
      })
    ).status(),
  ).toBe(401);
  await page
    .getByLabel("Lifecycle reason", { exact: true })
    .fill("Pause for inspection");
  await page
    .getByRole("button", { name: "Pause experiment", exact: true })
    .click();
  await expect(
    runs.getByRole("cell", { name: "paused", exact: true }),
  ).toBeVisible();
  await page
    .getByLabel("Lifecycle reason", { exact: true })
    .fill("Resume the same population");
  await page
    .getByRole("button", { name: "Resume experiment", exact: true })
    .click();
  await expect(
    runs.getByRole("cell", { name: "running", exact: true }),
  ).toBeVisible();
  await page
    .getByRole("combobox", { name: "Measurement cohort", exact: true })
    .selectOption("finalized");
  await expect(
    results.locator("tbody tr").first().locator("td").nth(2),
  ).toHaveText("0");
  await expect(
    page.getByText("Provisional counts can change as late events arrive.", {
      exact: false,
    }),
  ).toBeVisible();
  await page
    .getByLabel("Lifecycle reason", { exact: true })
    .fill("Close the inspected run");
  await page
    .getByRole("button", { name: "Complete experiment", exact: true })
    .click();
  await expect(
    runs.getByRole("cell", { name: "completed", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Resume experiment", exact: true }),
  ).toHaveCount(0);
  await page.screenshot({
    path: "../.cache/dashboard-experiments.png",
    fullPage: true,
  });
  await page.getByRole("button", { name: "Audit", exact: true }).click();
  await expect(
    page.getByText("Begin the development trial", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("Close the inspected run", { exact: true }),
  ).toBeVisible();
});

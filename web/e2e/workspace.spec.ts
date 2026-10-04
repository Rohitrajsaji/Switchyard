import { test, expect } from "@playwright/test";
test("admin signs in, creates a project, selects environments and reloads the session", async ({
  page,
}) => {
  const password = process.env.SWITCHYARD_DEMO_PASSWORD;
  if (!password)
    throw new Error(
      "Set SWITCHYARD_DEMO_PASSWORD and seed the local API before running browser tests.",
    );
  await page.goto("/");
  await page.getByLabel("Email", { exact: true }).fill("admin@example.test");
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Projects & environments" }),
  ).toBeVisible();
  const name = `Browser workspace ${Date.now()}`;
  await page.getByLabel("Project name", { exact: true }).fill(name);
  await page
    .getByRole("button", { name: "Create project", exact: true })
    .click();
  await expect(
    page
      .getByRole("combobox", { name: "Project", exact: true })
      .locator("option:checked"),
  ).toHaveText(name);
  await expect(
    page
      .getByRole("combobox", { name: "Environment", exact: true })
      .locator("option:checked"),
  ).toHaveText("development");
  await page.getByRole("button", { name: "Create flag", exact: true }).click();
  await page.getByLabel("Flag key", { exact: true }).fill("listing_flow");
  await page
    .getByText("Targeting and gradual rollout", { exact: true })
    .click();
  await page
    .getByLabel("Targeting rules (JSON)", { exact: true })
    .fill(
      '[{"attribute":"country","operator":"eq","values":["JP"],"value":{"type":"boolean","data":true}}]',
    );
  await page
    .getByLabel("Change reason", { exact: true })
    .fill("Target the Japan demo cohort");
  await page.getByRole("button", { name: "Save flag", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Evaluation preview" }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Evaluate user", exact: true })
    .click();
  await expect(page.getByText("targeting", { exact: true })).toBeVisible();
  await expect(page.getByLabel("Evaluation value")).toHaveText("true");
  await page.getByRole("button", { name: "Kill switch", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "Kill switch", exact: true }),
  ).toBeDisabled();
  await page
    .getByRole("button", { name: "Evaluate user", exact: true })
    .click();
  await expect(page.getByText("kill_switch", { exact: true })).toBeVisible();
  await expect(page.getByLabel("Evaluation value")).toHaveText("false");
  // This decimal cannot survive native JSON.parse/stringify without changing.
  await page.getByRole("button", { name: "Create flag", exact: true }).click();
  await page.getByLabel("Flag key", { exact: true }).fill("exact_config");
  await page
    .getByRole("combobox", { name: "Value type", exact: true })
    .selectOption("json");
  const precise = '{"threshold":0.123456789012345678901}';
  await page.getByLabel("Default value", { exact: true }).fill(precise);
  await page.getByLabel("Emergency safe value", { exact: true }).fill(precise);
  await page
    .getByLabel("Change reason", { exact: true })
    .fill("Preserve exact configuration decimals");
  await page.getByRole("button", { name: "Save flag", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Evaluation preview" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Edit flag", exact: true }).click();
  await expect(
    page.getByLabel("Emergency safe value", { exact: true }),
  ).toHaveValue(/0\.123456789012345678901/);
  await page
    .getByLabel("Change reason", { exact: true })
    .fill("Verify an exact-value edit");
  await page.getByRole("button", { name: "Save flag", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Evaluation preview" }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Evaluate user", exact: true })
    .click();
  await expect(page.getByLabel("Evaluation value")).toContainText(
    "0.123456789012345678901",
  );
  await page.getByRole("button", { name: "Edit flag", exact: true }).click();
  await page.getByLabel("Emergency safe value", { exact: true }).fill("null");
  await page
    .getByLabel("Change reason", { exact: true })
    .fill("Use an explicit JSON null safe value");
  await page.getByRole("button", { name: "Save flag", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Evaluation preview" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Edit flag", exact: true }).click();
  await expect(
    page.getByLabel("Emergency safe value", { exact: true }),
  ).toHaveValue("null");
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await page
    .getByRole("button", { name: "Evaluate user", exact: true })
    .click();
  await expect(page.getByLabel("Evaluation value")).toContainText(
    "0.123456789012345678901",
  );
  await page.screenshot({
    path: "../.cache/dashboard-flags.png",
    fullPage: true,
  });
  await page.getByRole("button", { name: "Audit", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Audit history" }),
  ).toBeVisible();
  await expect(
    page.getByText("Target the Japan demo cohort", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("Verify an exact-value edit", { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("combobox", { name: "Environment", exact: true })
    .selectOption({ label: "production" });
  await expect(
    page.getByText(
      "Production is read-only until reviewed change controls are available.",
    ),
  ).toBeVisible();
  await page.getByRole("button", { name: "Flags", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "Create flag", exact: true }),
  ).toHaveCount(0);
  await page.reload();
  await expect(
    page.getByRole("heading", { name: "Projects & environments" }),
  ).toBeVisible();
  await expect(
    page.getByText("admin@example.test", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "Sign in", exact: true }),
  ).toBeVisible();
});
test("viewer reads a member project's flags and audit but cannot mutate", async ({
  page,
}) => {
  const password = process.env.SWITCHYARD_DEMO_PASSWORD;
  if (!password) throw new Error("Set SWITCHYARD_DEMO_PASSWORD first.");
  // Arrange an independent project using the existing authoritative API.
  const login = await page.request.post("/api/backend/v1/session", {
    headers: { Origin: "http://localhost:3000" },
    data: { email: "admin@example.test", password },
  });
  expect(login.status()).toBe(200);
  const adminSession = await login.json();
  const headers = {
    Origin: "http://localhost:3000",
    "X-CSRF-Token": adminSession.csrf_token,
  };
  const name = `Viewer workspace ${Date.now()}`;
  const created = await page.request.post("/api/backend/v1/projects", {
    headers,
    data: { name },
  });
  expect(created.status()).toBe(201);
  const project = await created.json();
  const environments = await (
    await page.request.get(
      `/api/backend/v1/projects/${project.id}/environments`,
    )
  ).json();
  const environment = environments.find(
    (e: { name: string }) => e.name === "development",
  );
  const value = { type: "boolean", data: false };
  const flagBody = {
    environment_id: environment.id,
    key: "viewer_flag",
    type: "boolean",
    default: value,
    safe: value,
    reason: "Viewer read-only fixture",
  };
  let runID = "";
  expect(
    (
      await page.request.post(`/api/backend/v1/projects/${project.id}/flags`, {
        headers,
        data: flagBody,
      })
    ).status(),
  ).toBe(201);
  const runResponse = await page.request.post(
    `/api/backend/v1/projects/${project.id}/experiments`,
    {
      headers,
      data: {
        environment_id: environment.id,
        flag_key: "viewer_flag",
        expected_revision: 1,
        name: "Viewer experiment",
        control_variant_id: "control",
        traffic_bp: 10000,
        variants: [
          { id: "control", ordinal: 0, weight_bp: 5000, value },
          {
            id: "treatment",
            ordinal: 1,
            weight_bp: 5000,
            value: { type: "boolean", data: true },
          },
        ],
        reason: "Read-only experiment fixture",
      },
    },
  );
  expect(runResponse.status()).toBe(201);
  runID = (await runResponse.json()).id;
  expect(
    (
      await page.request.post(
        `/api/backend/v1/projects/${project.id}/members`,
        { headers, data: { user_id: "demo_viewer" } },
      )
    ).status(),
  ).toBe(204);
  expect(
    (
      await page.request.delete("/api/backend/v1/session", { headers })
    ).status(),
  ).toBe(204);
  await page.goto("/");
  await page.getByLabel("Email", { exact: true }).fill("viewer@example.test");
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(
    page.getByText(
      "Viewer access: you can inspect project data. Configuration changes require a developer or admin.",
    ),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Create project", exact: true }),
  ).toHaveCount(0);
  await expect(page.getByRole("option", { name, exact: true })).toBeAttached();
  await page
    .getByRole("combobox", { name: "Project", exact: true })
    .selectOption({ label: name });
  await expect(
    page.getByRole("button", { name: "viewer_flag", exact: true }),
  ).toBeVisible();
  for (const control of ["Create flag", "Edit flag", "Kill switch"]) {
    await expect(
      page.getByRole("button", { name: control, exact: true }),
    ).toHaveCount(0);
  }
  await page
    .getByRole("button", { name: "Evaluate user", exact: true })
    .click();
  await expect(page.getByLabel("Evaluation value")).toHaveText("false");
  const viewerSession = await (
    await page.request.get("/api/backend/v1/session")
  ).json();
  const denied = await page.request.post(
    `/api/backend/v1/projects/${project.id}/flags`,
    {
      headers: {
        Origin: "http://localhost:3000",
        "X-CSRF-Token": viewerSession.csrf_token,
        "X-Role": "admin",
      },
      data: { ...flagBody, key: "forged_viewer_write" },
    },
  );
  expect(denied.status()).toBe(403);
  await page.getByRole("button", { name: "Experiments", exact: true }).click();
  await expect(
    page
      .getByRole("table", { name: "Variant conversion results" })
      .locator("tbody tr"),
  ).toHaveCount(2);
  await expect(
    page.getByRole("button", { name: "Create experiment", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Start experiment", exact: true }),
  ).toHaveCount(0);
  expect(
    (
      await page.request.post(
        `/api/backend/v1/projects/${project.id}/experiments/${runID}/transitions`,
        {
          headers: {
            Origin: "http://localhost:3000",
            "X-CSRF-Token": viewerSession.csrf_token,
          },
          data: {
            action: "start",
            expected_revision: 1,
            reason: "Forbidden viewer transition",
          },
        },
      )
    ).status(),
  ).toBe(403);
  await page.getByRole("button", { name: "Audit", exact: true }).click();
  await expect(
    page.getByText("Viewer read-only fixture", { exact: true }),
  ).toBeVisible();
});

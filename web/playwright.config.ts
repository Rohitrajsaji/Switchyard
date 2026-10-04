import { defineConfig, devices } from "@playwright/test";
const baseURL = process.env.SWITCHYARD_WEB_URL ?? "http://localhost:3000";
export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [["list"], ["html", { open: "never" }]],
  use: {
    baseURL,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer:
    process.env.SWITCHYARD_E2E_EXTERNAL === "true"
      ? undefined
      : {
          command: "npm run dev",
          url: baseURL,
          reuseExistingServer: !process.env.CI,
          timeout: 120000,
        },
});

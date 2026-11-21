import { defineConfig, devices } from "@playwright/test";
import { staffOrigin } from "./e2e/staff";
import { databaseAs } from "./test-support/database";

// E2E_PRODUCTION=1 runs the suite against an optimized build instead of the dev server,
// which catches differences such as static rendering and minified hydration.
const production = process.env.E2E_PRODUCTION === "1";

export default defineConfig({
  forbidOnly: !!process.env.CI,
  testDir: "./e2e",
  timeout: 45_000,
  expect: { timeout: 15_000 },
  fullyParallel: false,
  workers: 1,
  reporter: [["list"]],
  use: {
    baseURL: "http://127.0.0.1:3000",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [
    { name: "desktop-chromium", use: { ...devices["Desktop Chrome"] } },
    { name: "mobile-chromium", use: { ...devices["Pixel 7"] } },
    { name: "tablet-chromium", use: { ...devices["Desktop Chrome"], viewport: { width: 768, height: 1024 } } },
  ],
  webServer: [
    {
      command: "cd ../donor-api && go run ./cmd/api",
      env: {
        DONOR_STAFF_ORIGIN: staffOrigin,
        DONOR_PUBLIC_ORIGIN: "http://127.0.0.1:3000",
        DONOR_GO_LISTEN: "127.0.0.1:8002",
        ...databaseAs("dad_api"),
      },
      url: "http://127.0.0.1:8002/health/live",
      reuseExistingServer: false,
      timeout: 30_000,
    },
    {
      command: production ? "npm run build && npm run start" : "npm run dev -- --hostname 127.0.0.1 --port 3000",
      env: {
        HOSTNAME: "127.0.0.1",
        PORT: "3000",
        DONOR_STAFF_ORIGIN: staffOrigin,
        DONOR_PUBLIC_ORIGIN: "http://127.0.0.1:3000",
        DONOR_API_BASE_URL: "http://127.0.0.1:8002",
      },
      url: "http://127.0.0.1:3000",
      reuseExistingServer: false,
      timeout: production ? 300_000 : 30_000,
    },
  ],
});

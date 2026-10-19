import { defineConfig } from '@playwright/test'

const openAIKey = process.env.E2E_OPENAI_DISABLED === '1' ? '' : 'fixture-openai-key'
const apiURL = process.env.E2E_API_URL ?? 'http://127.0.0.1:18080'
const fakeURL = process.env.E2E_FAKE_URL ?? 'http://127.0.0.1:18090'
const webURL = process.env.E2E_WEB_URL ?? 'http://127.0.0.1:4173'
const databaseURL = process.env.E2E_DATABASE_URL ?? 'postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_e2e?sslmode=disable'
const api = new URL(apiURL)
const fake = new URL(fakeURL)
const web = new URL(webURL)
const fixtureSteamID = `7656119${String(Date.now()).slice(-10)}`

export default defineConfig({
  testDir: './tests/e2e',
  timeout: 120_000,
  expect: { timeout: 20_000 },
  workers: 1,
  retries: 0,
  outputDir: process.env.E2E_OUTPUT_DIR ?? 'test-results',
  use: { baseURL: webURL, trace: 'retain-on-failure' },
  projects: [{ name: 'chromium', use: { browserName: 'chromium', launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : undefined } }],
  webServer: [
    { command: `cd .. && TEST_HARNESS_MODE=1 E2E_FAKE_ADDR=${fake.host} GOCACHE=/tmp/next-game-crush-e2e-go-cache GOWORK=off go run ./cmd/e2e-fakes`, url: `${fakeURL}/health`, reuseExistingServer: false, timeout: 120_000 },
    { command: `cd .. && TEST_HARNESS_MODE=1 FIXTURE_AUTH_ENABLED=1 DEMO_MODE_ENABLED=1 HTTP_ADDR=${api.host} APP_BASE_URL=${webURL} ALLOWED_STEAM_ID=${fixtureSteamID} STEAM_WEB_API_KEY=fixture-steam-key OPENAI_API_KEY='${openAIKey}' OPENAI_MODEL=gpt-6-luna OPENAI_EMBEDDING_MODEL=text-embedding-3-small OPENAI_BASE_URL=${fakeURL} STEAM_WEB_API_BASE_URL=${fakeURL} STEAM_STORE_BASE_URL=${fakeURL} STEAM_STORE_SERVICE_BASE_URL=${fakeURL} CATALOG_DISCOVERY_ENABLED=1 CATALOG_WORKER_CONCURRENCY=1 CATALOG_APPDETAILS_HOURLY_BUDGET=10000 CATALOG_AI_HOURLY_BUDGET=10000 CATALOG_RECONCILIATION_PAGE_SIZE=5 CATALOG_SEARCH_PAGE_SIZE=2 DATABASE_URL=${databaseURL} SMTP_URL=smtp://127.0.0.1:1025 EMAIL_ACTION_SECRET=fixture-email-action-secret SESSION_SECRET=fixture-session-secret GOCACHE=/tmp/next-game-crush-e2e-go-cache GOWORK=off go run ./cmd/next-game-crush`, url: `${apiURL}/api/health`, reuseExistingServer: false, timeout: 120_000 },
    { command: `VITE_API_TARGET=${apiURL} pnpm dev --host ${web.hostname} --port ${web.port}`, url: webURL, reuseExistingServer: false, timeout: 120_000 },
  ],
})

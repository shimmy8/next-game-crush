import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: './tests/e2e',
  timeout: 120_000,
  expect: { timeout: 20_000 },
  workers: 1,
  retries: 0,
  use: { baseURL: 'http://127.0.0.1:4173', trace: 'retain-on-failure' },
  projects: [{ name: 'chromium', use: { browserName: 'chromium', launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : undefined } }],
  webServer: [
    { command: 'cd .. && GOWORK=off go run ./cmd/e2e-fakes', url: 'http://127.0.0.1:18090/health', reuseExistingServer: false, timeout: 120_000 },
    { command: 'cd .. && TEST_HARNESS_MODE=1 FIXTURE_AUTH_ENABLED=1 HTTP_ADDR=127.0.0.1:18080 APP_BASE_URL=http://127.0.0.1:4173 ALLOWED_STEAM_ID=76561198000000000 STEAM_WEB_API_KEY=fixture-steam-key OPENAI_API_KEY=fixture-openai-key OPENAI_MODEL=gpt-6-luna OPENAI_EMBEDDING_MODEL=text-embedding-3-small OPENAI_BASE_URL=http://127.0.0.1:18090 STEAM_WEB_API_BASE_URL=http://127.0.0.1:18090 STEAM_STORE_BASE_URL=http://127.0.0.1:18090 DATABASE_URL=postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_e2e?sslmode=disable SMTP_URL=smtp://127.0.0.1:1025 EMAIL_ACTION_SECRET=fixture-email-action-secret SESSION_SECRET=fixture-session-secret GOWORK=off go run ./cmd/next-game-crush', url: 'http://127.0.0.1:18080/api/health', reuseExistingServer: false, timeout: 120_000 },
    { command: 'VITE_API_TARGET=http://127.0.0.1:18080 pnpm dev --host 127.0.0.1 --port 4173', url: 'http://127.0.0.1:4173', reuseExistingServer: false, timeout: 120_000 },
  ],
})

# Emergency Jobs Switch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an emergency setting that keeps HTTP available while preventing all background workers and schedulers from running.

**Architecture:** Parse one strict, default-enabled `JOBS_ENABLED` flag into application configuration. Carry it into the application lifecycle and conditionally omit the interactive worker and catalog runtime while leaving HTTP lifecycle and shutdown behavior unchanged.

**Tech Stack:** Go 1.27.2, Docker Compose, PostgreSQL-backed job queue

**Spec:** `docs/superpowers/specs/2026-10-20-emergency-jobs-switch-design.md`

## Global Constraints

- Use `GOWORK=off` for direct Go commands.
- Do not delete, rewrite, or execute existing queued jobs while emergency mode is active.
- Do not expose credentials or read deployment `.env` contents.
- Keep `JOBS_ENABLED` enabled by default for backward compatibility.

## Review Focus

- Empty setting preserves enabled behavior; cover in configuration tests.
- Both supported false values disable jobs; cover in configuration tests.
- Both supported true values enable jobs; cover in configuration tests.
- Invalid values fail startup with the setting name; cover in configuration tests.
- HTTP cancellation still shuts down cleanly without starting any runner; cover in lifecycle tests.

---

### Task 1: Configuration contract

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Config.JobsEnabled bool`

- [ ] **Step 1: Write failing tests for default, accepted values, and invalid input.**
- [ ] **Step 2: Run `GOWORK=off go test ./internal/config` and confirm the new tests fail.**
- [ ] **Step 3: Parse `JOBS_ENABLED` strictly with an enabled default.**
- [ ] **Step 4: Run `GOWORK=off go test -race ./internal/config` and confirm it passes.**

### Task 2: HTTP-only lifecycle

**Files:**
- Modify: `internal/app/app.go`
- Test: `internal/app/store_retry_test.go`

**Interfaces:**
- Consumes: `config.Config.JobsEnabled bool`
- Produces: `App.jobsDisabled bool` and conditional component startup in `(*App).Run`

- [ ] **Step 1: Write a failing lifecycle test proving no runner starts while HTTP remains active.**
- [ ] **Step 2: Run the focused test and confirm it fails.**
- [ ] **Step 3: Build the runtime component list from the flag, always including HTTP and including job runtimes only when enabled.**
- [ ] **Step 4: Run `GOWORK=off go test -race ./internal/app` and confirm it passes.**

### Task 3: Deployment wiring and release gate

**Files:**
- Modify: `.env.example`
- Modify: `docker-compose.yml`
- Modify: `compose.prod.yml`
- Modify: `README.md`

**Interfaces:**
- Consumes: `JOBS_ENABLED`
- Produces: Compose pass-through with `${JOBS_ENABLED:-1}` and operator documentation

- [ ] **Step 1: Add the documented variable and Compose pass-through.**
- [ ] **Step 2: Run `docker compose config --quiet` and production Compose config validation.**
- [ ] **Step 3: Run `codegraph sync .` and `codegraph affected` for changed source files.**
- [ ] **Step 4: Run one fresh `make verify` release gate.**
- [ ] **Step 5: Review the complete diff, commit, push, and create the pull request.**

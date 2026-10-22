# dev-lease e2e specs

Playwright specs for the dashboard (`internal/dashboard`): the pages
render at desktop and phone widths in light and dark themes without
horizontal scroll and without serious axe violations, the dialogs stop
and remove real stacks, the 15 s refresh keeps polling, and a refresh
never touches the Keep select on the stack page. Every matrix
combination leaves a screenshot in `shots/` (override with
`DEV_LEASE_E2E_SHOTS`); the CI workflow uploads them as an artifact.

## What the specs assume

A live serve on `DEV_LEASE_E2E_URL` (default `http://127.0.0.1:8410`)
managing `devlease-it-demo-*` only, with two stacks up. No other dev-lease
server may run on the same Docker (serve refuses to start next to one), and
port 8410 must be free on the host.

- `devlease-it-demo-a`: one kept service on its own volume, one plain
  service (`fixtures/demo-a/compose.yml`),
- `devlease-it-demo-b`: one plain service (`fixtures/demo-b/compose.yml`),
  already **stopped** (containers stopped, claims released, the sweep has
  recorded the stop).

The specs run in one worker and in file order on purpose: they share the
serve and the flows change the stacks' state.

## Local run

1. From the repository root, set serve's environment for this shell (the
   example's compose file requires `DEV_LEASE_PROJECTS`, and every compose
   command below, `down` included, needs it), build the image and start the
   serve over the example config:

   ```sh
   export DEV_LEASE_PROJECTS='devlease-it-demo-*' DEV_LEASE_INTERVAL=5s
   docker build -t devlease-it-e2e:local .
   docker compose -f examples/basic/compose.yml -f e2e/serve.override.yml -p devlease-it-e2e-serve up -d --wait
   ```

   The serve publishes the dashboard on 8410, manages only
   `devlease-it-demo-*` (its own project does not match) and sweeps every
   5 s.

2. Start the demo stacks and put demo-b into its stopped state:

   ```sh
   docker compose -f e2e/fixtures/demo-a/compose.yml -p devlease-it-demo-a up -d --wait
   docker compose -f e2e/fixtures/demo-b/compose.yml -p devlease-it-demo-b up -d --wait
   # wait until the dashboard shows both stacks, then
   docker compose -f e2e/fixtures/demo-b/compose.yml -p devlease-it-demo-b stop
   docker exec "$(docker ps -qf label=dev-lease.role=server)" dev-lease release devlease-it-demo-b --all
   # wait one sweep for the dashboard to show demo-b stopped
   ```

3. Run the specs from this directory:

   ```sh
   cd e2e
   npm ci
   npx playwright test
   ```

   The browser must be installed once: `npx playwright install chromium`.

4. Back in the repository root, with the same environment, tear everything
   down and confirm the daemon is clean:

   ```sh
   docker compose -f examples/basic/compose.yml -f e2e/serve.override.yml -p devlease-it-e2e-serve down -v --remove-orphans
   docker compose -f e2e/fixtures/demo-a/compose.yml -p devlease-it-demo-a down -v --remove-orphans
   docker compose -f e2e/fixtures/demo-b/compose.yml -p devlease-it-demo-b down -v --remove-orphans
   docker rmi devlease-it-e2e:local
   ```

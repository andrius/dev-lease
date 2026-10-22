# Release checklist

Run this before tagging a release. CI covers Linux (build, vet, race tests,
the integration test, the examples, the Docker image); these are the checks
CI cannot do.

## 0. CI on GitHub

1. Push the branch; the CI workflow passes on GitHub.

## 1. macOS and Windows (Docker Desktop)

Do each step on both. On PowerShell write
`docker exec (docker ps -qf label=dev-lease.role=server) dev-lease ...`.

1. Copy `examples/basic/.env.example` to `examples/basic/.env` with
   `DEV_LEASE_PROJECTS=devlease-check` and `DEV_LEASE_REMOVE_AFTER=1h` (the
   shortest delay serve accepts), then start serve:

   ```bash
   docker compose -p dev-lease -f examples/basic/compose.yml up -d --wait
   ```

2. Start a scratch stack under that name (any small compose file with one
   service and one named volume):
   `docker compose -p devlease-check up -d`, plus a second stack under
   another name that serve must leave alone.
3. `docker exec "$(docker ps -qf label=dev-lease.role=server)" dev-lease list`
   shows `devlease-check` with a live default lease.
4. End the lease at once: `... dev-lease release devlease-check --all`.
   Within a minute the scratch containers are stopped (`docker ps -a`) and
   its volume and network still exist; `list` says `stopped ... removed in
   59m`.
5. One hour later the scratch stack is gone: no container, network, volume
   or built image with its label. The second stack is untouched.
6. `docker compose -p dev-lease -f examples/basic/compose.yml down`, then
   `up -d --wait` again: `list` still shows the state.

Note: a native Windows client build has no `flock`; do not point it at a
state file that a running serve uses. Use `docker exec` there.

## 2. The image

After the tag push has built the image:

1. Make the `ghcr.io/andrius/dev-lease` package public (package
   settings, visibility). Pull the image logged out
   (`docker logout ghcr.io` first).
2. `docker run --rm ghcr.io/andrius/dev-lease:v0.1.0` prints the usage.
3. `docker run --rm ghcr.io/andrius/dev-lease:v0.1.0 -version` prints
   `dev-lease v0.1.0`.
4. `gh attestation verify oci://ghcr.io/andrius/dev-lease:v0.1.0 --owner andrius`
   passes (gh 2.49 or later).
5. The package page on GitHub shows the description, and
   `docker buildx imagetools inspect ghcr.io/andrius/dev-lease:v0.1.0 --format '{{json .Manifest.Annotations}}'`
   lists the description, version, revision and source annotations.

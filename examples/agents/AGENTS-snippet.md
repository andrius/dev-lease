## dev-lease

This repo's compose project is leased by [dev-lease](https://github.com/andrius/dev-lease):
idle projects are stopped, then removed (volumes included) after a grace
period. Work within the lease:

- Extend before starting or using the stack: run the hook from
  `.claude/hooks/lease-extend.sh` (it is wired into `.claude/settings.json`)
  or `docker exec "$(docker ps -qf label=dev-lease.role=server)" dev-lease extend <project> <ttl> --holder <name>`.
  Use a TTL matching the job (e.g. `6h`); never extend a stack you are not
  using right now.
- `docker exec "$(docker ps -qf label=dev-lease.role=server)" dev-lease list`
  shows every lease and what happens next (time left, or stopped how long
  ago and when the removal runs); `status` adds what each project has on
  the daemon.
- A stopped project is not lost: `docker compose -p <project> up -d --wait`
  brings it back and re-arms the lease. What was removed is gone for real,
  volumes included - rebuild instead of restoring.

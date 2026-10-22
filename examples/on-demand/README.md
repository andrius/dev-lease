# on-demand

The compose file for a machine that should not keep a docker service up
all the time (a laptop): serve exits once no managed project has a live
lease or a running container that is not kept and, with a removal delay above 0, nothing
stopped is waiting for removal.

Use `examples/basic` (always-on) on servers and dev boxes, this one on
laptops.

The compose file takes its variables from a `.env` file next to it
(Compose reads `.env` from the compose file's directory): copy
`.env.example` to `.env` and edit it, do not set the variables inline in
the command. An exited serve container is restarted as it is and is never
recreated by the scripts (`up -d --no-recreate --wait`), so after
changing a variable, recreate the container once yourself:
`docker compose -p dev-lease -f <the compose file> up -d --force-recreate`.

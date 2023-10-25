# Dockerized Jepsen

This docker image attempts to simplify the setup required by Jepsen.
It is intended to be used by a CI tool or anyone with Docker who wants to try Jepsen themselves.

It contains all the jepsen dependencies and code. It uses [Docker
Compose](https://github.com/docker/compose) to spin up the five containers used
by Jepsen.

## Quickstart

Assuming you have `docker compose` set up already, run:

```
docker compose -p jepsen -f docker-compose.yml up
```

Start a console on the control node

```
docker exec -ti jepsen-control /bin/bash
```

Your DB nodes are `n1`, `n2`, `n3`, `n4`, and `n5`.

## Advanced

If you need to log into a DB node (e.g. to debug a test), you can `ssh n1` (or n2, n3, ...) from inside the control node, or:

```
docker exec -ti jepsen-n1 bash
```
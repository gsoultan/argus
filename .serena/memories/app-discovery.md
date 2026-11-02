# What runs on a server

The agent says what runs on its host: known applications (Postgres, Redis,
RabbitMQ, nginx, Docker... the catalogue in `internal/agent/apps.go`), every
listening TCP socket with its process and systemd unit, running services, and
containers. It rides `/api/v1/report/facts` as `applications`, is stored in
`agents.applications` (JSONB, migration 031) and is served by
`GET /api/v1/assets/{id}/applications` (fleet roles only). The server page's
"What runs here" card shows it; a host Postgres has "Add as database", which
opens the add form with its port and any PgBouncer's port filled in.

## Read-only, and never a secret

- Sources (Linux): `/proc/net/tcp{,6}`, `/proc/<pid>/fd` links to name a
  socket's owner, `/proc/<pid>/comm` and the `exe` link's basename, the
  pid's cgroup for its unit, `/sys/fs/cgroup/system.slice` for running
  services, dpkg's status file / Alpine's installed db / `rpm -qa`, and
  `GET /containers/json` on the Docker or Podman socket.
- Never read: a process's `cmdline` or `environ`, config files, container
  labels or env (`/containers/json` carries no env; labels are dropped in
  `decodeContainers`). Never connects to what it finds. A test plants a
  password in a fake `cmdline` and checks nothing the scan returns carries it; the live
  check ran a `postgres` container with `POSTGRES_PASSWORD` set and the
  stored row did not contain it.
- Only catalogue packages are kept, so a host's full package list never
  leaves it.

## Decisions

- Ambiguous runtimes (java, beam.smp) are never named by process alone; their
  unit, package or image says what they are.
- A container's processes appear in the host's `/proc`; `inContainer` skips
  them so a containerised Postgres is not also reported as a host one. Ports
  a container publishes belong to the container, whatever host process
  (docker-proxy) holds them.
- A containerised Postgres gets no "Add as database": Argus opens databases as
  the host's `postgres` account over its local socket, which a container
  does not share.
- Scanned every 10 minutes, sent when it changed or hourly; an old agent's
  facts (no `applications`) leave the stored inventory alone (COALESCE).
- The report is bounded on the way in (`AppInventory.bounded`).
- Not on Windows/macOS yet beyond the container socket.

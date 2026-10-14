# redis-cluster-connection-breakdown

This demo shows how to break down Redis Cluster connections by client name (and by client IP) in Grafana, in near real time.

```
 redis-app-1 ─┐                                  ┌─ redis-node-1 (primary) ─ redis-node-4/5/6 (replicas)
 redis-app-2 ─┼─ go-redis ClusterClient pools ──▶│  redis-node-2 (primary)
 redis-app-3 ─┘  (CLIENT SETNAME = app name)     └─ redis-node-3 (primary)
   (manual)                                                │ CLIENT LIST
                                                           ▼
                        Grafana ◀── VictoriaMetrics ◀── redis_exporter (/scrape?target=…)
```

| Component | Image | Port |
|---|---|---|
| Redis Cluster (3 primaries + 3 replicas) | `redis:8.6.7` | internal |
| Go apps (go-redis v9 connection pool) | built from [`app/`](app/) | – |
| redis_exporter (`--export-client-list`) | `oliver006/redis_exporter:v1.92.1` | 9121 |
| VictoriaMetrics (single node, scrapes every 5s) | `victoriametrics/victoria-metrics:v1.153.0` | 8428 |
| Grafana (anonymous admin, provisioned dashboard) | `grafana/grafana:12.4.13` | 3000 |

## How it works

- Each app creates a `redis.ClusterClient` with `ClientName: "redis-app-N"`. go-redis then runs `CLIENT SETNAME` on every connection it opens. go-redis keeps a separate pool (`PoolSize`, `MinIdleConns`) for each cluster node. Because reads go to replicas too (`ReadOnly` + `RouteRandomly`), each app connects to all six nodes.
- redis_exporter runs with `REDIS_EXPORTER_EXPORT_CLIENT_LIST=true`. It turns `CLIENT LIST` into `redis_connected_client_info{name, host, flags, ...}`, one series per connection.
- VictoriaMetrics scrapes each node through the exporter's multi-target `/scrape` endpoint ([`victoriametrics/scrape.yml`](victoriametrics/scrape.yml)).
- The dashboard counts series: `count by (name) (redis_connected_client_info)`. The `client_name` variable is filled from the label values, so a new client name shows up without editing the dashboard.

## Run it

```bash
docker compose up -d --build
```

Open http://localhost:3000. The **Redis Cluster – Connection Breakdown** dashboard is the home page and refreshes every 5s.

Panels:
- Current connections per client, and the total
- Connections by client name over time (stacked)
- Connections by client name × cluster node, as a time series and as a matrix table
- Connections by client name × node role (primary or replica)
- Connections by client name × client IP
- `redis_connected_clients` per node, plus unnamed connections (such as replication links)

## Add `redis-app-3` manually

`redis-app-3` is in the `manual` profile, so `docker compose up` doesn't start it. Start it yourself:

```bash
docker compose up -d redis-app-3
```

By default it adds one worker per second (`RAMP_INTERVAL=1s`, up to 60 workers, pool size 40 per node), so its connections climb gradually on the dashboard. Stop it again with:

```bash
docker compose stop redis-app-3
```

### Tuning the pools

Each app reads these environment variables, and you can override them from the shell (or from `.env`):

| App variable | Compose override | Meaning |
|---|---|---|
| `POOL_SIZE` | `APPn_POOL_SIZE` | max connections per cluster node |
| `MIN_IDLE_CONNS` | `APPn_MIN_IDLE_CONNS` | warm idle connections per node |
| `WORKERS` | `APPn_WORKERS` | concurrent goroutines issuing commands |
| `RAMP_INTERVAL` | `APPn_RAMP_INTERVAL` | delay between starting workers (`0s` = all at once) |
| `OP_DELAY` | – | pause between commands per worker (default `20ms`) |

Example: `APP3_POOL_SIZE=100 APP3_WORKERS=150 docker compose up -d redis-app-3`.

Every 10s, each app also logs its own pool stats (`docker compose logs -f redis-app-1`), so you can compare them with what Redis reports.

## Useful checks

```bash
# Named connections as Redis sees them
docker compose exec redis-node-1 redis-cli client list | grep -o 'name=[^ ]*' | sort | uniq -c

# Raw exporter output for one node
curl -s 'localhost:9121/scrape?target=redis://redis-node-1:6379' | grep connected_client_info

# Query VictoriaMetrics directly
curl -s localhost:8428/api/v1/query --data-urlencode 'query=count by (name) (redis_connected_client_info)'
```

## Tear down

```bash
docker compose --profile manual down -v
```

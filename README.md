# KWatch — Kafka Consumer Lag Monitoring and Alerting

KWatch reports how far behind your Kafka consumer groups are, and alerts your
team **while they are still catching up** rather than after the backlog has
already aged out of retention.

Lag for a partition is the distance between the end of the log and the offset a
consumer group has committed:

```
lag = log end offset - committed offset
```

KWatch polls the cluster for both numbers, exports the difference per group,
topic and partition to Prometheus, renders it in Grafana, and ships alert rules
that fire on the *trend* as well as on absolute thresholds.

## Quick start

```bash
docker compose up -d
```

| Service | URL | What is there |
|---|---|---|
| Grafana | http://localhost:3000 | The consumer lag dashboard, pre-provisioned |
| Prometheus | http://localhost:9091 | Alert rules under **Alerts** |
| KWatch | http://localhost:9090/metrics | Raw metrics |
| Kafka | `localhost:9092` | Single-node KRaft broker |

To see lag appear, produce some messages and consume only part of them:

```bash
K=/opt/kafka/bin
docker exec kwatch-kafka $K/kafka-topics.sh --bootstrap-server kafka:9092 \
  --create --topic orders --partitions 6 --replication-factor 1

seq 1 1200 | docker exec -i kwatch-kafka $K/kafka-console-producer.sh \
  --bootstrap-server kafka:9092 --topic orders

docker exec kwatch-kafka $K/kafka-console-consumer.sh \
  --bootstrap-server kafka:9092 --topic orders --group order_consumer_1 \
  --from-beginning --max-messages 500 --timeout-ms 20000

curl -s localhost:9090/api/lag
```

KWatch will report a lag of 700 on partition 0, matching what
`kafka-consumer-groups.sh --describe` reports.

## Running KWatch on its own

```bash
go run ./cmd/kwatch --brokers localhost:9092
```

### Configuration

Every setting has a `KWATCH_*` environment variable; the three most common also
have flags, which take precedence.

| Variable | Flag | Default | Meaning |
|---|---|---|---|
| `KWATCH_BROKERS` | `--brokers` | `localhost:9092` | Comma-separated bootstrap addresses |
| `KWATCH_LISTEN_ADDR` | `--listen` | `:9090` | HTTP bind address |
| `KWATCH_POLL_INTERVAL` | `--poll-interval` | `15s` | How often lag is collected |
| `KWATCH_POLL_TIMEOUT` | — | `10s` | Bound on a single collection round |
| `KWATCH_TOPICS` | — | all topics | Restrict to these topics |
| `KWATCH_GROUPS` | — | all groups | Restrict to these consumer groups |

Kafka's internal topics (`__consumer_offsets` and friends) are always skipped.

### Endpoints

| Path | Purpose |
|---|---|
| `/metrics` | Prometheus exposition |
| `/healthz` | Liveness probe |
| `/api/lag` | The last snapshot as JSON, handy when debugging by hand |

## Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `kwatch_consumer_group_lag` | `group`, `topic`, `partition` | Messages behind the end of the log |
| `kwatch_consumer_group_committed_offset` | `group`, `topic`, `partition` | Last committed offset |
| `kwatch_topic_partition_end_offset` | `topic`, `partition` | Log end offset |
| `kwatch_up` | — | `1` if the last round reached Kafka |
| `kwatch_scrapes_total` | — | Collection rounds attempted |
| `kwatch_scrape_errors_total` | — | Collection rounds that failed |
| `kwatch_scrape_duration_seconds` | — | Duration of the last round |
| `kwatch_last_scrape_timestamp_seconds` | — | Unix time of the last success |

Two behaviours are worth knowing:

- **Stale series are dropped.** The lag gauges are reset each round, so a group
  that goes away stops being exported instead of freezing at its last value.
- **A failed round keeps the last known lag published** and sets `kwatch_up` to
  `0`. Alerting keys on `kwatch_up`, so a monitoring outage is never mistaken
  for a healthy cluster.

## Alerts

Defined in [`deploy/prometheus/alerts.yml`](deploy/prometheus/alerts.yml) and
unit-tested in [`alerts_test.yml`](deploy/prometheus/alerts_test.yml).

| Alert | Severity | Fires when |
|---|---|---|
| `KafkaConsumerLagGrowing` | warning | Lag is *trending* toward 10k within 30 minutes |
| `KafkaConsumerGroupStalled` | critical | Topic is still receiving but the group stopped committing |
| `KafkaConsumerLagHigh` | warning | Lag over 10k for 10 minutes |
| `KafkaConsumerLagCritical` | critical | Lag over 100k for 5 minutes |
| `KWatchDown` | critical | Prometheus cannot scrape KWatch |
| `KWatchCannotReachKafka` | critical | KWatch is up but cannot reach the cluster |
| `KWatchSlowCollection` | warning | Collection is approaching the poll interval |

`KafkaConsumerLagGrowing` is the one that does the real work. It uses
`predict_linear` over a 15-minute window, so it pages while the group is still
catching up. The rule test covers exactly this: on a steadily climbing backlog
it fires at around 46 minutes, while actual lag is still near 7.7k — well
before the 10k absolute threshold would have said anything.

## Tests

```bash
go test -race ./...                        # unit tests, no broker needed
docker compose up -d --wait kafka          # then, against a real broker:
go test -tags=integration ./test/...
```

The integration suite builds a real fixture — topics, produced messages, and
committed offsets — then asserts the collector's lag matches the expected value
for **every** group, topic and partition.

Its shape is configurable rather than hardcoded, so the suite is not pinned to
one cluster size:

```bash
KWATCH_IT_TOPICS=3 KWATCH_IT_PARTITIONS=64 KWATCH_IT_GROUPS=12 \
  go test -tags=integration ./test/...
```

| Variable | Default |
|---|---|
| `KWATCH_IT_BROKERS` | `localhost:9092` |
| `KWATCH_IT_TOPICS` | `2` |
| `KWATCH_IT_PARTITIONS` | `20` |
| `KWATCH_IT_GROUPS` | `5` |
| `KWATCH_IT_MESSAGES` | `40` per partition |

Alert rules are tested with `promtool`:

```bash
docker run --rm -v "$PWD/deploy/prometheus:/p:ro" -w /p \
  --entrypoint promtool prom/prometheus:v3.1.0 test rules alerts_test.yml
```

## CI

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs on every push and
pull request:

1. **Unit** — `gofmt`, `go vet`, `go test -race` with coverage
2. **Alert rules** — `promtool` config check and rule unit tests
3. **Integration** — a real Kafka broker from `compose.yaml`, at two cluster
   shapes (20 partitions / 5 groups, and 64 partitions / 12 groups)
4. **Full stack** — builds the image and asserts KWatch reaches Kafka,
   Prometheus scrapes KWatch, and Grafana provisions the dashboard

CI starts Kafka from the same `compose.yaml` developers run locally, so the two
cannot drift apart.

## Project structure

```
KWatch
├── cmd/kwatch/              service entry point and HTTP handlers
├── internal/
│   ├── config/              KWATCH_* configuration and validation
│   ├── lag/                 the collector: topics, groups, offsets, lag
│   └── metrics/             Prometheus collectors
├── test/                    integration tests (build tag: integration)
├── deploy/
│   ├── prometheus/          scrape config, alert rules, rule tests
│   └── grafana/             provisioned datasource and dashboard
├── .github/workflows/ci.yml
├── compose.yaml
└── Dockerfile
```

## Prerequisites

```
docker
go 1.26+
```

//go:build integration

// Package integration exercises the lag collector against a real Kafka broker.
//
//	docker compose up -d kafka
//	go test -tags=integration ./test/...
//
// The shape of the cluster is configurable so the suite is not pinned to one
// size. Defaults are 2 topics x 20 partitions x 5 consumer groups; raise them
// with the KWATCH_IT_* variables to test a wider fan-out:
//
//	KWATCH_IT_PARTITIONS=64 KWATCH_IT_GROUPS=12 go test -tags=integration ./test/...
package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/mkdirRON/KWatch/internal/lag"
	"github.com/mkdirRON/KWatch/internal/metrics"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// clusterShape is the size of the fixture built for one test run.
type clusterShape struct {
	brokers            string
	topics             int
	partitions         int
	groups             int
	messagesPerPartion int
}

func shapeFromEnv() clusterShape {
	return clusterShape{
		brokers:            envString("KWATCH_IT_BROKERS", "localhost:9092"),
		topics:             envInt("KWATCH_IT_TOPICS", 2),
		partitions:         envInt("KWATCH_IT_PARTITIONS", 20),
		groups:             envInt("KWATCH_IT_GROUPS", 5),
		messagesPerPartion: envInt("KWATCH_IT_MESSAGES", 40),
	}
}

func envString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		panic(fmt.Sprintf("%s must be a positive integer, got %q", key, v))
	}
	return n
}

// explicitPartition routes each message to the partition named on it. The
// Writer always consults a balancer, so targeting a partition needs one.
type explicitPartition struct{}

func (explicitPartition) Balance(msg kafka.Message, _ ...int) int { return msg.Partition }

// fixture is the cluster state one test run builds and asserts against.
type fixture struct {
	shape  clusterShape
	client *kafka.Client
	topics []string
	groups []string

	// wantLag is the lag each group/topic/partition must report.
	wantLag map[string]int64
}

func key(group, topic string, partition int) string {
	return group + "|" + topic + "|" + strconv.Itoa(partition)
}

// setup builds topics, produces to every partition, and commits a different
// offset per group so each group has a distinct, known lag.
func setup(t *testing.T, shape clusterShape) *fixture {
	t.Helper()

	client := &kafka.Client{Addr: kafka.TCP(shape.brokers), Timeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// A per-run suffix keeps repeated runs from inheriting old offsets.
	run := strconv.FormatInt(time.Now().UnixNano(), 36)

	f := &fixture{shape: shape, client: client, wantLag: map[string]int64{}}
	for i := 0; i < shape.topics; i++ {
		f.topics = append(f.topics, fmt.Sprintf("kwatch_it_%s_topic_%d", run, i))
	}
	for g := 0; g < shape.groups; g++ {
		f.groups = append(f.groups, fmt.Sprintf("kwatch_it_%s_group_%d", run, g))
	}

	createTopics(ctx, t, client, f.topics, shape.partitions)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := client.DeleteTopics(cleanupCtx, &kafka.DeleteTopicsRequest{Topics: f.topics}); err != nil {
			t.Logf("cleanup: delete topics: %v", err)
		}
	})

	produce(ctx, t, shape, f.topics)
	commitOffsets(ctx, t, client, f)
	return f
}

func createTopics(ctx context.Context, t *testing.T, client *kafka.Client, topics []string, partitions int) {
	t.Helper()

	configs := make([]kafka.TopicConfig, 0, len(topics))
	for _, name := range topics {
		configs = append(configs, kafka.TopicConfig{
			Topic:             name,
			NumPartitions:     partitions,
			ReplicationFactor: 1,
		})
	}

	resp, err := client.CreateTopics(ctx, &kafka.CreateTopicsRequest{Topics: configs})
	if err != nil {
		t.Fatalf("create topics: %v", err)
	}
	for name, err := range resp.Errors {
		if err != nil {
			t.Fatalf("create topic %s: %v", name, err)
		}
	}

	// Topic metadata propagates asynchronously; wait for every partition to
	// have a leader before producing.
	deadline := time.Now().Add(60 * time.Second)
	for {
		md, err := client.Metadata(ctx, &kafka.MetadataRequest{Topics: topics})
		if err == nil && metadataReady(md, topics, partitions) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("topics did not become ready within 60s (last error: %v)", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func metadataReady(md *kafka.MetadataResponse, topics []string, partitions int) bool {
	seen := 0
	for _, topic := range md.Topics {
		if topic.Error != nil || len(topic.Partitions) != partitions {
			continue
		}
		for _, p := range topic.Partitions {
			if p.Error != nil || p.Leader.Host == "" {
				return false
			}
		}
		seen++
	}
	return seen == len(topics)
}

func produce(ctx context.Context, t *testing.T, shape clusterShape, topics []string) {
	t.Helper()

	writer := &kafka.Writer{
		Addr:                   kafka.TCP(shape.brokers),
		Balancer:               explicitPartition{},
		AllowAutoTopicCreation: false,
		BatchSize:              500,
		RequiredAcks:           kafka.RequireAll,
		WriteTimeout:           30 * time.Second,
	}
	defer writer.Close()

	msgs := make([]kafka.Message, 0, shape.partitions*shape.messagesPerPartion)
	for _, topic := range topics {
		for p := 0; p < shape.partitions; p++ {
			for i := 0; i < shape.messagesPerPartion; i++ {
				msgs = append(msgs, kafka.Message{
					Topic:     topic,
					Partition: p,
					Value:     []byte(strconv.Itoa(i)),
				})
			}
		}
	}
	if err := writer.WriteMessages(ctx, msgs...); err != nil {
		t.Fatalf("produce %d messages: %v", len(msgs), err)
	}
}

// commitOffsets gives each group a different position in the log, so a bug
// that mixed groups up would produce the wrong lag rather than a plausible one.
func commitOffsets(ctx context.Context, t *testing.T, client *kafka.Client, f *fixture) {
	t.Helper()

	end := int64(f.shape.messagesPerPartion)
	for g, group := range f.groups {
		commits := map[string][]kafka.OffsetCommit{}
		for _, topic := range f.topics {
			for p := 0; p < f.shape.partitions; p++ {
				// Group 0 is fully caught up; each later group sits one more
				// step back, and the partition index skews it further so no
				// two partitions share a lag by accident.
				behind := int64(g) + int64(p%3)
				if behind > end {
					behind = end
				}
				committed := end - behind

				commits[topic] = append(commits[topic], kafka.OffsetCommit{
					Partition: p,
					Offset:    committed,
				})
				f.wantLag[key(group, topic, p)] = behind
			}
		}

		commit(ctx, t, client, group, commits)
	}
}

// commit writes offsets for a group, retrying while the broker is still
// electing a group coordinator. A freshly started cluster returns
// NotCoordinatorForGroup until __consumer_offsets is ready, which would
// otherwise make this suite flaky on every clean CI run.
func commit(ctx context.Context, t *testing.T, client *kafka.Client, group string, commits map[string][]kafka.OffsetCommit) {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	for attempt := 1; ; attempt++ {
		resp, err := client.OffsetCommit(ctx, &kafka.OffsetCommitRequest{
			GroupID: group,
			// -1 commits as an admin tool would, without joining the group.
			GenerationID: -1,
			Topics:       commits,
		})
		if err == nil {
			err = firstPartitionError(resp)
		}
		if err == nil {
			return
		}
		if !retriableCoordinatorError(err) || time.Now().After(deadline) {
			t.Fatalf("commit offsets for %s (attempt %d): %v", group, attempt, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func firstPartitionError(resp *kafka.OffsetCommitResponse) error {
	for topic, partitions := range resp.Topics {
		for _, p := range partitions {
			if p.Error != nil {
				return fmt.Errorf("%s/%d: %w", topic, p.Partition, p.Error)
			}
		}
	}
	return nil
}

func retriableCoordinatorError(err error) bool {
	for _, code := range []kafka.Error{
		kafka.NotCoordinatorForGroup,
		kafka.GroupCoordinatorNotAvailable,
		kafka.GroupLoadInProgress,
		kafka.RebalanceInProgress,
	} {
		if errors.Is(err, code) {
			return true
		}
	}
	return false
}

// collect runs the collector against the fixture, restricted to this run's
// topics and groups so a shared broker's other data cannot affect the result.
func (f *fixture) collect(t *testing.T) lag.Snapshot {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	snap, err := lag.New(f.client, f.topics, f.groups).Collect(ctx)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return snap
}

func TestCollectorAgainstRealCluster(t *testing.T) {
	shape := shapeFromEnv()
	t.Logf("cluster: %d topics x %d partitions x %d groups, %d messages per partition",
		shape.topics, shape.partitions, shape.groups, shape.messagesPerPartion)

	f := setup(t, shape)
	snap := f.collect(t)

	wantEntries := shape.topics * shape.partitions * shape.groups
	if len(snap.Entries) != wantEntries {
		t.Fatalf("entries = %d, want %d (%d topics x %d partitions x %d groups)",
			len(snap.Entries), wantEntries, shape.topics, shape.partitions, shape.groups)
	}

	wantPartitions := shape.topics * shape.partitions
	if len(snap.Partitions) != wantPartitions {
		t.Errorf("partitions = %d, want %d", len(snap.Partitions), wantPartitions)
	}

	end := int64(shape.messagesPerPartion)
	seen := make(map[string]bool, wantEntries)
	for _, e := range snap.Entries {
		k := key(e.Group, e.Topic, e.Partition)
		want, ok := f.wantLag[k]
		if !ok {
			t.Errorf("unexpected entry %s", k)
			continue
		}
		if seen[k] {
			t.Errorf("duplicate entry %s", k)
		}
		seen[k] = true

		if e.Lag != want {
			t.Errorf("%s: lag = %d, want %d", k, e.Lag, want)
		}
		if e.EndOffset != end {
			t.Errorf("%s: end offset = %d, want %d", k, e.EndOffset, end)
		}
		if e.CommittedOffset != end-want {
			t.Errorf("%s: committed = %d, want %d", k, e.CommittedOffset, end-want)
		}
	}
	for k := range f.wantLag {
		if !seen[k] {
			t.Errorf("missing entry %s", k)
		}
	}
}

// TestCollectorTracksConsumerCatchingUp checks that lag falls as a group
// commits forward, which is the behaviour the growth alert reads.
func TestCollectorTracksConsumerCatchingUp(t *testing.T) {
	shape := shapeFromEnv()
	f := setup(t, shape)

	before := f.collect(t)
	lagFor := func(snap lag.Snapshot, group, topic string, partition int) int64 {
		for _, e := range snap.Entries {
			if e.Group == group && e.Topic == topic && e.Partition == partition {
				return e.Lag
			}
		}
		t.Fatalf("no entry for %s/%s/%d", group, topic, partition)
		return -1
	}

	// Pick a group that is behind, then commit it to the end of the log.
	group, topic := f.groups[len(f.groups)-1], f.topics[0]
	if got := lagFor(before, group, topic, 1); got == 0 {
		t.Fatalf("fixture group %s should start behind on partition 1", group)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	commits := make([]kafka.OffsetCommit, 0, shape.partitions)
	for p := 0; p < shape.partitions; p++ {
		commits = append(commits, kafka.OffsetCommit{
			Partition: p,
			Offset:    int64(shape.messagesPerPartion),
		})
	}
	commit(ctx, t, f.client, group, map[string][]kafka.OffsetCommit{topic: commits})

	after := f.collect(t)
	for p := 0; p < shape.partitions; p++ {
		if got := lagFor(after, group, topic, p); got != 0 {
			t.Errorf("partition %d: lag = %d after catching up, want 0", p, got)
		}
	}
	// The other groups must be untouched by one group committing.
	other := f.groups[0]
	for p := 0; p < shape.partitions; p++ {
		want := f.wantLag[key(other, topic, p)]
		if got := lagFor(after, other, topic, p); got != want {
			t.Errorf("group %s partition %d: lag = %d, want %d (unchanged)", other, p, got, want)
		}
	}
}

// TestMetricsExportRealSnapshot checks the Prometheus layer publishes one
// series per group/topic/partition at this fan-out.
func TestMetricsExportRealSnapshot(t *testing.T) {
	shape := shapeFromEnv()
	f := setup(t, shape)

	reg := prometheus.NewRegistry()
	metrics.New(reg).Observe(f.collect(t))

	wantSeries := shape.topics * shape.partitions * shape.groups
	if got := testutil.CollectAndCount(reg, "kwatch_consumer_group_lag"); got != wantSeries {
		t.Errorf("lag series = %d, want %d", got, wantSeries)
	}
	if got := testutil.CollectAndCount(reg, "kwatch_topic_partition_end_offset"); got != shape.topics*shape.partitions {
		t.Errorf("end offset series = %d, want %d", got, shape.topics*shape.partitions)
	}
	if got := testutil.CollectAndCount(reg, "kwatch_up"); got != 1 {
		t.Errorf("kwatch_up series = %d, want 1", got)
	}
}

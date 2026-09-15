// Package lag collects Kafka consumer group lag from a cluster.
//
// Lag for a partition is the distance between the end of the log and the
// offset a consumer group has committed:
//
//	lag = log end offset - committed offset
//
// A group that keeps up sits near zero; a group falling behind climbs.
package lag

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
)

// Client is the subset of *kafka.Client that the collector needs. Narrowing it
// to an interface keeps the collector testable without a live broker.
type Client interface {
	Metadata(context.Context, *kafka.MetadataRequest) (*kafka.MetadataResponse, error)
	ListGroups(context.Context, *kafka.ListGroupsRequest) (*kafka.ListGroupsResponse, error)
	ListOffsets(context.Context, *kafka.ListOffsetsRequest) (*kafka.ListOffsetsResponse, error)
	OffsetFetch(context.Context, *kafka.OffsetFetchRequest) (*kafka.OffsetFetchResponse, error)
}

// Entry is the lag of one consumer group on one topic partition.
type Entry struct {
	Group           string `json:"group"`
	Topic           string `json:"topic"`
	Partition       int    `json:"partition"`
	CommittedOffset int64  `json:"committed_offset"`
	EndOffset       int64  `json:"end_offset"`
	Lag             int64  `json:"lag"`
}

// PartitionOffset is the log end offset of one topic partition, reported
// independently of any consumer group.
type PartitionOffset struct {
	Topic     string `json:"topic"`
	Partition int    `json:"partition"`
	EndOffset int64  `json:"end_offset"`
}

// Snapshot is the result of one collection round.
type Snapshot struct {
	Entries     []Entry           `json:"entries"`
	Partitions  []PartitionOffset `json:"partitions"`
	CollectedAt time.Time         `json:"collected_at"`
	Duration    time.Duration     `json:"duration"`
}

// Collector reads consumer group lag from a Kafka cluster.
type Collector struct {
	client Client

	// topicFilter and groupFilter restrict what is collected. A nil filter
	// means "everything the cluster reports".
	topicFilter map[string]bool
	groupFilter map[string]bool
}

// New builds a Collector. Empty topics or groups mean no filtering.
func New(client Client, topics, groups []string) *Collector {
	return &Collector{
		client:      client,
		topicFilter: toSet(topics),
		groupFilter: toSet(groups),
	}
}

// Collect runs one round: discover topics and groups, read log end offsets,
// then read each group's committed offsets and diff them.
func (c *Collector) Collect(ctx context.Context) (Snapshot, error) {
	start := time.Now()

	partitions, err := c.topicPartitions(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list topic partitions: %w", err)
	}
	if len(partitions) == 0 {
		return Snapshot{CollectedAt: start, Duration: time.Since(start)}, nil
	}

	ends, err := c.endOffsets(ctx, partitions)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list end offsets: %w", err)
	}

	groups, err := c.consumerGroups(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list consumer groups: %w", err)
	}

	snap := Snapshot{Partitions: flattenEnds(ends), CollectedAt: start}
	for _, group := range groups {
		entries, err := c.groupLag(ctx, group, partitions, ends)
		if err != nil {
			// One unavailable group (a rebalance, a dead coordinator) must
			// not blank out the metrics for every other group.
			continue
		}
		snap.Entries = append(snap.Entries, entries...)
	}

	sort.Slice(snap.Entries, func(i, j int) bool {
		a, b := snap.Entries[i], snap.Entries[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Topic != b.Topic {
			return a.Topic < b.Topic
		}
		return a.Partition < b.Partition
	})

	snap.Duration = time.Since(start)
	return snap, nil
}

// topicPartitions returns the partitions of every monitored topic.
func (c *Collector) topicPartitions(ctx context.Context) (map[string][]int, error) {
	resp, err := c.client.Metadata(ctx, &kafka.MetadataRequest{})
	if err != nil {
		return nil, err
	}

	out := make(map[string][]int)
	for _, topic := range resp.Topics {
		if topic.Error != nil || !c.watchTopic(topic.Name) {
			continue
		}
		ids := make([]int, 0, len(topic.Partitions))
		for _, p := range topic.Partitions {
			ids = append(ids, p.ID)
		}
		sort.Ints(ids)
		if len(ids) > 0 {
			out[topic.Name] = ids
		}
	}
	return out, nil
}

// endOffsets reads the log end offset of every given partition.
func (c *Collector) endOffsets(ctx context.Context, partitions map[string][]int) (map[string]map[int]int64, error) {
	req := &kafka.ListOffsetsRequest{Topics: make(map[string][]kafka.OffsetRequest, len(partitions))}
	for topic, ids := range partitions {
		reqs := make([]kafka.OffsetRequest, 0, len(ids))
		for _, id := range ids {
			reqs = append(reqs, kafka.LastOffsetOf(id))
		}
		req.Topics[topic] = reqs
	}

	resp, err := c.client.ListOffsets(ctx, req)
	if err != nil {
		return nil, err
	}

	out := make(map[string]map[int]int64, len(resp.Topics))
	for topic, offsets := range resp.Topics {
		for _, p := range offsets {
			if p.Error != nil {
				continue
			}
			if out[topic] == nil {
				out[topic] = make(map[int]int64)
			}
			out[topic][p.Partition] = p.LastOffset
		}
	}
	return out, nil
}

// consumerGroups returns the monitored consumer groups on the cluster.
func (c *Collector) consumerGroups(ctx context.Context) ([]string, error) {
	resp, err := c.client.ListGroups(ctx, &kafka.ListGroupsRequest{})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}

	var out []string
	for _, g := range resp.Groups {
		// Connect and other non-consumer protocols do not track topic lag.
		if g.ProtocolType != "" && g.ProtocolType != "consumer" {
			continue
		}
		if c.watchGroup(g.GroupID) {
			out = append(out, g.GroupID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// groupLag diffs one group's committed offsets against the log end offsets.
func (c *Collector) groupLag(ctx context.Context, group string, partitions map[string][]int, ends map[string]map[int]int64) ([]Entry, error) {
	resp, err := c.client.OffsetFetch(ctx, &kafka.OffsetFetchRequest{
		GroupID: group,
		Topics:  partitions,
	})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}

	var out []Entry
	for topic, committed := range resp.Topics {
		for _, p := range committed {
			if p.Error != nil {
				continue
			}
			// A negative committed offset means the group has never
			// committed on this partition, so it has no lag to report.
			if p.CommittedOffset < 0 {
				continue
			}
			end, ok := ends[topic][p.Partition]
			if !ok {
				continue
			}
			// A commit can briefly run ahead of the end offset we read a
			// moment earlier; clamp rather than report negative lag.
			delta := end - p.CommittedOffset
			if delta < 0 {
				delta = 0
			}
			out = append(out, Entry{
				Group:           group,
				Topic:           topic,
				Partition:       p.Partition,
				CommittedOffset: p.CommittedOffset,
				EndOffset:       end,
				Lag:             delta,
			})
		}
	}
	if len(out) == 0 {
		return nil, errNoOffsets
	}
	return out, nil
}

var errNoOffsets = errors.New("group has no committed offsets")

func (c *Collector) watchTopic(name string) bool {
	if strings.HasPrefix(name, "__") {
		return false // Kafka internals: __consumer_offsets and friends.
	}
	if c.topicFilter == nil {
		return true
	}
	return c.topicFilter[name]
}

func (c *Collector) watchGroup(id string) bool {
	if c.groupFilter == nil {
		return true
	}
	return c.groupFilter[id]
}

// flattenEnds turns the nested end-offset map into a sorted slice.
func flattenEnds(ends map[string]map[int]int64) []PartitionOffset {
	var out []PartitionOffset
	for topic, byPartition := range ends {
		for id, offset := range byPartition {
			out = append(out, PartitionOffset{Topic: topic, Partition: id, EndOffset: offset})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Topic != out[j].Topic {
			return out[i].Topic < out[j].Topic
		}
		return out[i].Partition < out[j].Partition
	})
	return out
}

func toSet(values []string) map[string]bool {
	if len(values) == 0 {
		return nil
	}
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return set
}

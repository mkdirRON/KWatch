package lag

import (
	"context"
	"errors"
	"testing"

	"github.com/segmentio/kafka-go"
)

// fakeClient is a scripted stand-in for *kafka.Client.
type fakeClient struct {
	topics    map[string]int           // topic -> partition count
	endOffset map[string]map[int]int64 // topic -> partition -> log end offset
	groups    []kafka.ListGroupsResponseGroup
	committed map[string]map[string]map[int]int64 // group -> topic -> partition -> committed

	metadataErr error
	groupsErr   error
	offsetErr   map[string]error // group -> error from OffsetFetch

	offsetFetchCalls int
}

func (f *fakeClient) Metadata(_ context.Context, _ *kafka.MetadataRequest) (*kafka.MetadataResponse, error) {
	if f.metadataErr != nil {
		return nil, f.metadataErr
	}
	resp := &kafka.MetadataResponse{}
	for name, count := range f.topics {
		topic := kafka.Topic{Name: name}
		for i := 0; i < count; i++ {
			topic.Partitions = append(topic.Partitions, kafka.Partition{ID: i, Topic: name})
		}
		resp.Topics = append(resp.Topics, topic)
	}
	return resp, nil
}

func (f *fakeClient) ListGroups(_ context.Context, _ *kafka.ListGroupsRequest) (*kafka.ListGroupsResponse, error) {
	if f.groupsErr != nil {
		return nil, f.groupsErr
	}
	return &kafka.ListGroupsResponse{Groups: f.groups}, nil
}

func (f *fakeClient) ListOffsets(_ context.Context, req *kafka.ListOffsetsRequest) (*kafka.ListOffsetsResponse, error) {
	resp := &kafka.ListOffsetsResponse{Topics: map[string][]kafka.PartitionOffsets{}}
	for topic, offsets := range req.Topics {
		for _, o := range offsets {
			end, ok := f.endOffset[topic][o.Partition]
			if !ok {
				continue
			}
			resp.Topics[topic] = append(resp.Topics[topic], kafka.PartitionOffsets{
				Partition:  o.Partition,
				LastOffset: end,
			})
		}
	}
	return resp, nil
}

func (f *fakeClient) OffsetFetch(_ context.Context, req *kafka.OffsetFetchRequest) (*kafka.OffsetFetchResponse, error) {
	f.offsetFetchCalls++
	if err := f.offsetErr[req.GroupID]; err != nil {
		return nil, err
	}
	resp := &kafka.OffsetFetchResponse{Topics: map[string][]kafka.OffsetFetchPartition{}}
	for topic, byPartition := range f.committed[req.GroupID] {
		for partition, offset := range byPartition {
			resp.Topics[topic] = append(resp.Topics[topic], kafka.OffsetFetchPartition{
				Partition:       partition,
				CommittedOffset: offset,
			})
		}
	}
	return resp, nil
}

func newFake() *fakeClient {
	return &fakeClient{
		topics:    map[string]int{"orders": 2, "__consumer_offsets": 1},
		endOffset: map[string]map[int]int64{"orders": {0: 100, 1: 250}},
		groups: []kafka.ListGroupsResponseGroup{
			{GroupID: "order_consumer_1", ProtocolType: "consumer"},
		},
		committed: map[string]map[string]map[int]int64{
			"order_consumer_1": {"orders": {0: 90, 1: 250}},
		},
	}
}

func TestCollectComputesLag(t *testing.T) {
	snap, err := New(newFake(), nil, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snap.Entries) != 2 {
		t.Fatalf("entries = %d, want 2: %+v", len(snap.Entries), snap.Entries)
	}
	// Entries are sorted by group, topic, then partition.
	if got, want := snap.Entries[0].Lag, int64(10); got != want {
		t.Errorf("partition 0 lag = %d, want %d", got, want)
	}
	if got, want := snap.Entries[1].Lag, int64(0); got != want {
		t.Errorf("partition 1 lag = %d, want %d (caught up)", got, want)
	}
	if got, want := snap.Entries[0].EndOffset, int64(100); got != want {
		t.Errorf("end offset = %d, want %d", got, want)
	}
	if len(snap.Partitions) != 2 {
		t.Errorf("partitions = %d, want 2", len(snap.Partitions))
	}
}

func TestCollectSkipsInternalTopics(t *testing.T) {
	f := newFake()
	f.endOffset["__consumer_offsets"] = map[int]int64{0: 5}
	snap, err := New(f, nil, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, p := range snap.Partitions {
		if p.Topic == "__consumer_offsets" {
			t.Fatal("internal topic should not be reported")
		}
	}
}

func TestCollectClampsNegativeLag(t *testing.T) {
	f := newFake()
	// A commit landing between our ListOffsets and OffsetFetch calls can read
	// ahead of the end offset; lag must not go negative.
	f.committed["order_consumer_1"]["orders"][0] = 120
	snap, err := New(f, nil, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := snap.Entries[0].Lag; got != 0 {
		t.Errorf("lag = %d, want 0", got)
	}
}

func TestCollectSkipsUncommittedPartitions(t *testing.T) {
	f := newFake()
	f.committed["order_consumer_1"]["orders"][1] = -1 // never committed
	snap, err := New(f, nil, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snap.Entries) != 1 {
		t.Fatalf("entries = %d, want 1: %+v", len(snap.Entries), snap.Entries)
	}
	if snap.Entries[0].Partition != 0 {
		t.Errorf("partition = %d, want 0", snap.Entries[0].Partition)
	}
}

func TestCollectFiltersTopicsAndGroups(t *testing.T) {
	f := newFake()
	f.topics["payments"] = 1
	f.endOffset["payments"] = map[int]int64{0: 10}
	f.groups = append(f.groups, kafka.ListGroupsResponseGroup{GroupID: "noisy", ProtocolType: "consumer"})
	f.committed["noisy"] = map[string]map[int]int64{"orders": {0: 1}}

	snap, err := New(f, []string{"orders"}, []string{"order_consumer_1"}).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, e := range snap.Entries {
		if e.Topic != "orders" || e.Group != "order_consumer_1" {
			t.Errorf("unfiltered entry: %+v", e)
		}
	}
	if f.offsetFetchCalls != 1 {
		t.Errorf("OffsetFetch calls = %d, want 1 (filtered group skipped)", f.offsetFetchCalls)
	}
}

func TestCollectSkipsNonConsumerProtocols(t *testing.T) {
	f := newFake()
	f.groups = append(f.groups, kafka.ListGroupsResponseGroup{GroupID: "connect-sink", ProtocolType: "connect"})
	f.committed["connect-sink"] = map[string]map[int]int64{"orders": {0: 1}}

	snap, err := New(f, nil, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, e := range snap.Entries {
		if e.Group == "connect-sink" {
			t.Fatal("connect group should not be reported")
		}
	}
}

func TestCollectSurvivesOneFailingGroup(t *testing.T) {
	f := newFake()
	f.groups = append(f.groups, kafka.ListGroupsResponseGroup{GroupID: "rebalancing", ProtocolType: "consumer"})
	f.offsetErr = map[string]error{"rebalancing": errors.New("coordinator not available")}

	snap, err := New(f, nil, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snap.Entries) != 2 {
		t.Fatalf("entries = %d, want the healthy group's 2", len(snap.Entries))
	}
}

func TestCollectFailsWhenClusterUnreachable(t *testing.T) {
	f := newFake()
	f.metadataErr = errors.New("dial tcp: connection refused")
	if _, err := New(f, nil, nil).Collect(context.Background()); err == nil {
		t.Fatal("expected an error when metadata fails")
	}
}

func TestCollectWithNoTopics(t *testing.T) {
	f := newFake()
	f.topics = nil
	snap, err := New(f, nil, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snap.Entries) != 0 {
		t.Errorf("entries = %d, want 0", len(snap.Entries))
	}
}

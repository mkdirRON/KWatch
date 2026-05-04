package main

import (
	"context"
	"sync"
	"github.com/segmentio/kafka-go"

)




type MetricsStore struct {
	LagMap map[string]int
	mu     sync.Mutex
}

// KakfaConfig configuration stuct for a kafka client
type KakfaConfig struct {
	topic string
	groupId string
	kafkaAddr string
}
    /
func kafkaPoll(metric *MetricsStore, kafkaConfig *KakfaConfig) error {
	client := kafka.Client{
		Addr: kafka.TCP(kafkaConfig.kafkaAddr),
	}

	offsetRequest := kafka.OffsetFetchRequest{
		GroupID: kafkaConfig.groupId,
		Topics: map[string][]int{
			kafkaConfig.topic: {0},
		},
	}

	resp, err := client.OffsetFetch(context.Background(), &offsetRequest)

	if err != nil {
		return err
	}
	return nil
	
}


func main() {

	metrics := MetricsStore{
		LagMap: make(map[string]int),
	}
	kafkaInstance := KakfaConfig{
		topic: "orders",
		groupId: "order_consumer_1",
		kafkaAddr: "localhost:9092",

	}
    kafkaIns := kafkaPoll(&LagMap, &kafkaInstance)
}





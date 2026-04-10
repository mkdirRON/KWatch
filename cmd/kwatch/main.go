package main

import "sync"

type MetricsStore struct {
	LagMap map[string]int
	mu     sync.Mutex
}

type KakfaConfig struct {
	topic string
	groupId string
	kafkaAddr string
}

func kafkaPoll(metric *MetricsStore, kafkaConfig *KakfaConfig) error {}

func main() {

	metrics := MetricsStore{
		LagMap: make(map[string]int),
	}
	kafkaInstance := KakfaConfig{
		topic: "orders",
		groupId: "order_consumer_1",
		kafkaAddr: "localhost:9092",
	}
}
package main


import (
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
	"github.com/segmentio/kafka-go"
)


func main() {

	fmt.Print("hello world!")
}
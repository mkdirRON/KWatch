# KWatch: A Kafka Consumer Lag Monitoring and Alert System 


### *Description* 

Kwatch is a Kafka monitoring system that reports and displays consumer lag, providing alerts when anomalies appear.  


### Prerequisite 
```yaml
docker
go 1.20+
```
### *Usage* 

#### **1.) init Kafka, Prometheus, and Grafana instances**
```yaml
docker compose up -d 
```
### **2.) Run main program 
```yaml
cd cmd/kwatch 
go run main.go
```





### *Current Project structure*
```bash 
    Kwatch 
    |--cmd
      |--kwatch
         |--main.go
    |--compose.yaml 
    |--go.mod
    |--go.sum 
    |--README.md
```

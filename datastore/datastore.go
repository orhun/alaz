package datastore

import (
	"github.com/ddosify/alaz/ebpf/l7_req"
)

type DataStore interface {
	PersistPod(pod Pod, eventType string) error
	PersistService(service Service, eventType string) error
	PersistReplicaSet(rs ReplicaSet, eventType string) error
	PersistDeployment(d Deployment, eventType string) error
	PersistEndpoints(e Endpoints, eventType string) error
	PersistContainer(c Container, eventType string) error
	PersistDaemonSet(ds DaemonSet, eventType string) error
	PersistStatefulSet(ss StatefulSet, eventType string) error

	PersistRequest(request *Request) error

	PersistKafkaEvent(request *KafkaEvent) error

	PersistTraceEvent(trace *l7_req.TraceEvent) error

	PersistAliveConnection(trace *AliveConnection) error
}

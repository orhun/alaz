package datastore

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ddosify/alaz/config"
	"github.com/ddosify/alaz/ebpf/l7_req"
	"github.com/ddosify/alaz/log"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log/level"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/rs/zerolog"

	"k8s.io/apimachinery/pkg/util/uuid"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"
	collector "github.com/prometheus/node_exporter/collector"

	poolutil "go.ddosify.com/ddosify/core/util"
)

var MonitoringID string
var NodeID string

// set from ldflags
var tag string
var kernelVersion string
var cloudProvider CloudProvider

func init() {

	TestMode := os.Getenv("TEST_MODE")
	if TestMode == "true" {
		return
	}

	MonitoringID = os.Getenv("MONITORING_ID")
	if MonitoringID == "" {
		log.Logger.Fatal().Msg("MONITORING_ID is not set")
	}

	NodeID = os.Getenv("NODE_NAME")
	if NodeID == "" {
		log.Logger.Fatal().Msg("NODE_NAME is not set")
	}

	if tag == "" {
		log.Logger.Fatal().Msg("tag is not set")
	}
	log.Logger.Info().Str("tag", tag).Msg("alaz tag")

	kernelVersion = extractKernelVersion()
	cloudProvider = getCloudProvider()
}

func extractKernelVersion() string {
	// Path to the /proc/version file
	filePath := "/proc/version"
	file, err := os.Open(filePath)
	if err != nil {
		log.Logger.Fatal().AnErr("error", err).Msgf("Unable to open file %s", filePath)
	}

	// Read the content of the file
	content, err := io.ReadAll(file)
	if err != nil {
		log.Logger.Fatal().AnErr("error", err).Msgf("Unable to read file %s", filePath)
	}

	// Convert the content to a string
	versionInfo := string(content)

	// Split the versionInfo string into lines
	lines := strings.Split(versionInfo, "\n")

	// Extract the kernel version from the first line
	// Assuming the kernel version is the first word in the first line
	if len(lines) > 0 {
		fields := strings.Fields(lines[0])
		if len(fields) > 2 {
			return fields[2]
		}
	}

	return "Unable to extract kernel version"
}

type CloudProvider string

const (
	CloudProviderAWS          CloudProvider = "AWS"
	CloudProviderGCP          CloudProvider = "GCP"
	CloudProviderAzure        CloudProvider = "Azure"
	CloudProviderDigitalOcean CloudProvider = "DigitalOcean"
	CloudProviderUnknown      CloudProvider = ""
)

func getCloudProvider() CloudProvider {
	if vendor, err := os.ReadFile("/sys/class/dmi/id/board_vendor"); err == nil {
		switch strings.TrimSpace(string(vendor)) {
		case "Amazon EC2":
			return CloudProviderAWS
		case "Google":
			return CloudProviderGCP
		case "Microsoft Corporation":
			return CloudProviderAzure
		case "DigitalOcean":
			return CloudProviderDigitalOcean
		}
	}
	return CloudProviderUnknown
}

var resourceBatchSize int64 = 50
var innerMetricsPort int = 8182

// BackendDS is a backend datastore
type BackendDS struct {
	ctx       context.Context
	host      string
	port      string
	c         *http.Client
	batchSize uint64

	reqChanBuffer  chan *ReqInfo
	connChanBuffer chan *ConnInfo
	reqInfoPool    *poolutil.Pool[*ReqInfo]
	aliveConnPool  *poolutil.Pool[*ConnInfo]

	traceEventQueue *list.List
	traceEventMu    sync.RWMutex

	traceInfoPool *poolutil.Pool[*TraceInfo]

	podEventChan       chan interface{} // *PodEvent
	svcEventChan       chan interface{} // *SvcEvent
	depEventChan       chan interface{} // *DepEvent
	rsEventChan        chan interface{} // *RsEvent
	epEventChan        chan interface{} // *EndpointsEvent
	containerEventChan chan interface{} // *ContainerEvent
	dsEventChan        chan interface{} // *DaemonSetEvent

	// TODO add:
	// statefulset
	// job
	// cronjob
}

const (
	podEndpoint       = "/alaz/k8s/pod/"
	svcEndpoint       = "/alaz/k8s/svc/"
	rsEndpoint        = "/alaz/k8s/replicaset/"
	depEndpoint       = "/alaz/k8s/deployment/"
	epEndpoint        = "/alaz/k8s/endpoint/"
	containerEndpoint = "/alaz/k8s/container/"
	dsEndpoint        = "/alaz/k8s/daemonset/"
	reqEndpoint       = "/alaz/"
	connEndpoint      = "/alaz/connections/"

	traceEventEndpoint = "/dist_tracing/traffic/"

	healthCheckEndpoint = "/alaz/healthcheck/"
)

type LeveledLogger struct {
	l zerolog.Logger
}

func (ll LeveledLogger) Error(msg string, keysAndValues ...interface{}) {
	ll.l.Error().Fields(keysAndValues).Msg(msg)
}
func (ll LeveledLogger) Info(msg string, keysAndValues ...interface{}) {
	ll.l.Info().Fields(keysAndValues).Msg(msg)
}
func (ll LeveledLogger) Debug(msg string, keysAndValues ...interface{}) {
	ll.l.Debug().Fields(keysAndValues).Msg(msg)
}
func (ll LeveledLogger) Warn(msg string, keysAndValues ...interface{}) {
	ll.l.Warn().Fields(keysAndValues).Msg(msg)
}

func NewBackendDS(parentCtx context.Context, conf config.BackendDSConfig) *BackendDS {
	ctx, _ := context.WithCancel(parentCtx)
	rand.Seed(time.Now().UnixNano())

	retryClient := retryablehttp.NewClient()
	retryClient.Logger = LeveledLogger{l: log.Logger.With().Str("component", "retryablehttp").Logger()}
	retryClient.Backoff = retryablehttp.DefaultBackoff
	retryClient.RetryWaitMin = 1 * time.Second
	retryClient.RetryWaitMax = 5 * time.Second
	retryClient.RetryMax = 4

	retryClient.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		var shouldRetry bool
		if err != nil { // resp, (doErr) = c.HTTPClient.Do(req.Request)
			// connection refused, connection reset, connection timeout
			shouldRetry = true
			log.Logger.Warn().Msgf("will retry, error: %v", err)
		} else {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusBadRequest ||
				resp.StatusCode == http.StatusTooManyRequests ||
				resp.StatusCode >= http.StatusInternalServerError {
				shouldRetry = true

				rb, err := io.ReadAll(resp.Body)
				if err != nil {
					log.Logger.Warn().Msgf("error reading response body: %v", err)
				}
				log.Logger.Warn().Int("statusCode", resp.StatusCode).
					Str("path", resp.Request.URL.Path).
					Str("respBody", string(rb)).Msgf("will retry...")
			} else if resp.StatusCode == http.StatusOK {
				shouldRetry = false
				rb, err := io.ReadAll(resp.Body)
				if err != nil {
					log.Logger.Debug().Msgf("error reading response body: %v", err)
				}

				// if req endpoint
				if resp.Request.URL.Path == reqEndpoint {
					var resp ReqBackendReponse
					err = json.Unmarshal(rb, &resp)
					if err != nil {
						log.Logger.Debug().Msgf("error unmarshalling response body: %v", err)
					}
					if len(resp.Errors) > 0 {
						for _, e := range resp.Errors {
							log.Logger.Error().Str("errorMsg", e.Error).Any("event", e.Event).Msgf("backend persist error")
						}
					}
				} else {
					var resp BackendResponse
					err = json.Unmarshal(rb, &resp)
					if err != nil {
						log.Logger.Debug().Msgf("error unmarshalling response body: %v", err)
					}
					if len(resp.Errors) > 0 {
						for _, e := range resp.Errors {
							log.Logger.Error().Str("errorMsg", e.Error).Any("event", e.Event).Msgf("backend persist error")
						}
					}
				}
			}
		}
		return shouldRetry, nil
	}

	retryClient.HTTPClient.Transport = &http.Transport{
		DisableKeepAlives: false,
		MaxConnsPerHost:   500, // 500 connection per host
	}
	retryClient.HTTPClient.Timeout = 10 * time.Second // Set a timeout for the request
	client := retryClient.StandardClient()

	var defaultBatchSize uint64 = 1000

	bs, err := strconv.ParseUint(os.Getenv("BATCH_SIZE"), 10, 64)
	if err != nil {
		bs = defaultBatchSize
	}

	ds := &BackendDS{
		ctx:                ctx,
		host:               conf.Host,
		c:                  client,
		batchSize:          bs,
		reqInfoPool:        newReqInfoPool(func() *ReqInfo { return &ReqInfo{} }, func(r *ReqInfo) {}),
		aliveConnPool:      newAliveConnPool(func() *ConnInfo { return &ConnInfo{} }, func(r *ConnInfo) {}),
		traceInfoPool:      newTraceInfoPool(func() *TraceInfo { return &TraceInfo{} }, func(r *TraceInfo) {}),
		reqChanBuffer:      make(chan *ReqInfo, conf.ReqBufferSize),
		connChanBuffer:     make(chan *ConnInfo, conf.ConnBufferSize),
		podEventChan:       make(chan interface{}, 100),
		svcEventChan:       make(chan interface{}, 100),
		rsEventChan:        make(chan interface{}, 100),
		depEventChan:       make(chan interface{}, 50),
		epEventChan:        make(chan interface{}, 100),
		containerEventChan: make(chan interface{}, 100),
		dsEventChan:        make(chan interface{}, 20),
		traceEventQueue:    list.New(),
	}

	go ds.sendReqsInBatch(bs)
	go ds.sendConnsInBatch(bs)
	go ds.sendTraceEventsInBatch(10 * bs)

	eventsInterval := 10 * time.Second
	go ds.sendEventsInBatch(ds.podEventChan, podEndpoint, eventsInterval)
	go ds.sendEventsInBatch(ds.svcEventChan, svcEndpoint, eventsInterval)
	go ds.sendEventsInBatch(ds.rsEventChan, rsEndpoint, eventsInterval)
	go ds.sendEventsInBatch(ds.depEventChan, depEndpoint, eventsInterval)
	go ds.sendEventsInBatch(ds.epEventChan, epEndpoint, eventsInterval)
	go ds.sendEventsInBatch(ds.containerEventChan, containerEndpoint, eventsInterval)
	go ds.sendEventsInBatch(ds.dsEventChan, dsEndpoint, eventsInterval)

	if conf.MetricsExport {
		go ds.exportNodeMetrics()

		go func() {
			t := time.NewTicker(time.Duration(conf.MetricsExportInterval) * time.Second)
			for {
				select {
				case <-ds.ctx.Done():
					return
				case <-t.C:
					// make a request to /inner/metrics
					// forward the response to /github.com/ddosify/alaz/metrics
					func() {
						req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://localhost:%d/inner/metrics", innerMetricsPort), nil)
						if err != nil {
							log.Logger.Error().Msgf("error creating inner metrics request: %v", err)
							return
						}

						ctx, cancel := context.WithTimeout(ds.ctx, 5*time.Second)
						defer cancel()

						// use the default client, ds client reads response on success to look for failed events,
						// therefore body here will be empty
						resp, err := http.DefaultClient.Do(req.WithContext(ctx))

						if err != nil {
							log.Logger.Error().Msgf("error sending inner metrics request: %v", err)
							return
						}

						if resp.StatusCode != http.StatusOK {
							log.Logger.Error().Msgf("inner metrics request not success: %d", resp.StatusCode)
							return
						}

						req, err = http.NewRequest(http.MethodPost, fmt.Sprintf("%s/alaz/metrics/scrape/?instance=%s&monitoring_id=%s", ds.host, NodeID, MonitoringID), resp.Body)
						if err != nil {
							log.Logger.Error().Msgf("error creating metrics request: %v", err)
							return
						}

						ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()

						resp, err = ds.c.Do(req.WithContext(ctx))

						if err != nil {
							log.Logger.Error().Msgf("error sending metrics request: %v", err)
							return
						}

						if resp.StatusCode != http.StatusOK {
							log.Logger.Error().Msgf("metrics request not success: %d", resp.StatusCode)

							// log response body
							rb, err := io.ReadAll(resp.Body)
							if err != nil {
								log.Logger.Error().Msgf("error reading metrics response body: %v", err)
							}
							log.Logger.Error().Msgf("metrics response body: %s", string(rb))

							return
						} else {
							log.Logger.Debug().Msg("metrics sent successfully")
						}
					}()
				}
			}
		}()
	}

	go func() {
		<-ds.ctx.Done()
		// TODO:
		// reqInfoPool.Put() results in send to closed channel if Done() is called
		// ds.reqInfoPool.Done()
		log.Logger.Info().Msg("backend datastore stopped")
	}()

	return ds
}

func (b *BackendDS) enqueueTraceInfo(traceInfo *TraceInfo) {
	b.traceEventMu.Lock()
	defer b.traceEventMu.Unlock()
	b.traceEventQueue.PushBack(traceInfo)
}

func (b *BackendDS) dequeueTraceEvents(batchSize uint64) []*TraceInfo {
	b.traceEventMu.Lock()
	defer b.traceEventMu.Unlock()

	batch := make([]*TraceInfo, 0, batchSize)

	for i := 0; i < int(batchSize); i++ {
		if b.traceEventQueue.Len() == 0 {
			return batch
		}

		elem := b.traceEventQueue.Front()
		b.traceEventQueue.Remove(elem)
		tInfo, _ := elem.Value.(*TraceInfo)

		batch = append(batch, tInfo)
	}

	return batch
}

func (b *BackendDS) DoRequest(req *http.Request) error {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	ctx, cancel := context.WithTimeout(b.ctx, 30*time.Second)
	defer cancel()

	resp, err := b.c.Do(req.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("error sending http request: %v", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body) // in order to reuse the connection
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("req failed: %d, %s", resp.StatusCode, string(body))
	}

	return nil
}

func convertReqsToPayload(batch []*ReqInfo) RequestsPayload {
	return RequestsPayload{
		Metadata: Metadata{
			MonitoringID:   MonitoringID,
			IdempotencyKey: string(uuid.NewUUID()),
			NodeID:         NodeID,
			AlazVersion:    tag,
		},
		Requests: batch,
	}
}

func convertConnsToPayload(batch []*ConnInfo) ConnInfoPayload {
	return ConnInfoPayload{
		Metadata: Metadata{
			MonitoringID:   MonitoringID,
			IdempotencyKey: string(uuid.NewUUID()),
			NodeID:         NodeID,
			AlazVersion:    tag,
		},
		Connections: batch,
	}
}

func convertTraceEventsToPayload(batch []*TraceInfo) TracePayload {
	return TracePayload{
		Metadata: Metadata{
			MonitoringID:   MonitoringID,
			IdempotencyKey: string(uuid.NewUUID()),
			NodeID:         NodeID,
			AlazVersion:    tag,
		},
		Traces: batch,
	}
}

func (b *BackendDS) sendToBackend(method string, payload interface{}, endpoint string) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Logger.Error().Msgf("error marshalling batch: %v", err)
		return
	}

	httpReq, err := http.NewRequest(method, b.host+endpoint, bytes.NewBuffer(payloadBytes))
	if err != nil {
		log.Logger.Error().Msgf("error creating http request: %v", err)
		return
	}

	log.Logger.Debug().Str("endpoint", endpoint).Any("payload", payload).Msg("sending batch to backend")
	err = b.DoRequest(httpReq)
	if err != nil {
		log.Logger.Error().Msgf("backend persist error at ep %s : %v", endpoint, err)
	}
}

func (b *BackendDS) sendTraceEventsInBatch(batchSize uint64) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()

	send := func() {
		batch := b.dequeueTraceEvents(batchSize)

		if len(batch) == 0 {
			return
		}

		tracePayload := convertTraceEventsToPayload(batch)
		go b.sendToBackend(http.MethodPost, tracePayload, traceEventEndpoint)

		// return reqInfoss to the pool
		for _, trace := range batch {
			b.traceInfoPool.Put(trace)
		}
	}

	for {
		select {
		case <-b.ctx.Done():
			log.Logger.Info().Msg("stopping sending trace events to backend")
			return
		case <-t.C:
			send()
		}
	}

}

func (b *BackendDS) sendReqsInBatch(batchSize uint64) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()

	send := func() {
		batch := make([]*ReqInfo, 0, batchSize)
		loop := true

		for i := 0; (i < int(batchSize)) && loop; i++ {
			select {
			case req := <-b.reqChanBuffer:
				batch = append(batch, req)
			case <-time.After(50 * time.Millisecond):
				loop = false
			}
		}

		if len(batch) == 0 {
			return
		}

		reqsPayload := convertReqsToPayload(batch)
		go b.sendToBackend(http.MethodPost, reqsPayload, reqEndpoint)

		// return reqInfoss to the pool
		for _, req := range batch {
			b.reqInfoPool.Put(req)
		}
	}

	for {
		select {
		case <-b.ctx.Done():
			log.Logger.Info().Msg("stopping sending reqs to backend")
			return
		case <-t.C:
			send()
		}
	}

}

func (b *BackendDS) sendConnsInBatch(batchSize uint64) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()

	send := func() {
		batch := make([]*ConnInfo, 0, batchSize)
		loop := true

		for i := 0; (i < int(batchSize)) && loop; i++ {
			select {
			case conn := <-b.connChanBuffer:
				batch = append(batch, conn)
			case <-time.After(50 * time.Millisecond):
				loop = false
			}
		}

		if len(batch) == 0 {
			return
		}

		connsPayload := convertConnsToPayload(batch)
		log.Logger.Info().Any("conns", connsPayload).Msgf("sending %d conns to backend", len(batch))
		go b.sendToBackend(http.MethodPost, connsPayload, connEndpoint)

		// return openConns to the pool
		for _, conn := range batch {
			b.aliveConnPool.Put(conn)
		}
	}

	for {
		select {
		case <-b.ctx.Done():
			log.Logger.Info().Msg("stopping sending reqs to backend")
			return
		case <-t.C:
			send()
		}
	}

}

func (b *BackendDS) send(ch <-chan interface{}, endpoint string) {
	batch := make([]interface{}, 0, resourceBatchSize)
	loop := true

	for i := 0; (i < int(resourceBatchSize)) && loop; i++ {
		select {
		case ev := <-ch:
			batch = append(batch, ev)
		case <-time.After(1 * time.Second):
			loop = false
		}
	}

	if len(batch) == 0 {
		return
	}

	payload := EventPayload{
		Metadata: Metadata{
			MonitoringID:   MonitoringID,
			IdempotencyKey: string(uuid.NewUUID()),
			NodeID:         NodeID,
			AlazVersion:    tag,
		},
		Events: batch,
	}

	b.sendToBackend(http.MethodPost, payload, endpoint)
}

func (b *BackendDS) sendEventsInBatch(ch chan interface{}, endpoint string, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-b.ctx.Done():
			log.Logger.Info().Msg("stopping sending events to backend")
			return
		case <-t.C:
			randomDuration := time.Duration(rand.Intn(50)) * time.Millisecond
			time.Sleep(randomDuration)

			b.send(ch, endpoint)
		}
	}
}

func newReqInfoPool(factory func() *ReqInfo, close func(*ReqInfo)) *poolutil.Pool[*ReqInfo] {
	return &poolutil.Pool[*ReqInfo]{
		Items:   make(chan *ReqInfo, 5000),
		Factory: factory,
		Close:   close,
	}
}

func newAliveConnPool(factory func() *ConnInfo, close func(*ConnInfo)) *poolutil.Pool[*ConnInfo] {
	return &poolutil.Pool[*ConnInfo]{
		Items:   make(chan *ConnInfo, 500),
		Factory: factory,
		Close:   close,
	}
}

func newTraceInfoPool(factory func() *TraceInfo, close func(*TraceInfo)) *poolutil.Pool[*TraceInfo] {
	return &poolutil.Pool[*TraceInfo]{
		Items:   make(chan *TraceInfo, 50000),
		Factory: factory,
		Close:   close,
	}
}

func (b *BackendDS) PersistAliveConnection(aliveConn *AliveConnection) error {
	// get a connInfo from the pool
	oc := b.aliveConnPool.Get()

	// overwrite the connInfo, all fields must be set in order to avoid conflict
	oc[0] = aliveConn.CheckTime
	oc[1] = aliveConn.FromIP
	oc[2] = aliveConn.FromType
	oc[3] = aliveConn.FromUID
	oc[4] = aliveConn.FromPort
	oc[5] = aliveConn.ToIP
	oc[6] = aliveConn.ToType
	oc[7] = aliveConn.ToUID
	oc[8] = aliveConn.ToPort

	b.connChanBuffer <- oc

	return nil
}

func (b *BackendDS) PersistRequest(request *Request) error {
	// get a reqInfo from the pool
	reqInfo := b.reqInfoPool.Get()

	// overwrite the reqInfo, all fields must be set in order to avoid conflict
	reqInfo[0] = request.StartTime
	reqInfo[1] = request.Latency
	reqInfo[2] = request.FromIP
	reqInfo[3] = request.FromType
	reqInfo[4] = request.FromUID
	reqInfo[5] = request.FromPort
	reqInfo[6] = request.ToIP
	reqInfo[7] = request.ToType
	reqInfo[8] = request.ToUID
	reqInfo[9] = request.ToPort
	reqInfo[10] = request.Protocol
	reqInfo[11] = request.StatusCode
	reqInfo[12] = request.FailReason // TODO ??
	reqInfo[13] = request.Method
	reqInfo[14] = request.Path
	reqInfo[15] = request.Tls
	reqInfo[16] = request.Seq
	reqInfo[17] = request.Tid

	b.reqChanBuffer <- reqInfo

	return nil
}

func (b *BackendDS) PersistTraceEvent(trace *l7_req.TraceEvent) error {
	if trace == nil {
		return fmt.Errorf("trace event is nil")
	}

	t := b.traceInfoPool.Get()

	t[0] = trace.Tx
	t[1] = trace.Seq
	t[2] = trace.Tid

	ingress := false      // EGRESS
	if trace.Type_ == 0 { // INGRESS
		ingress = true
	}

	t[3] = ingress

	b.enqueueTraceInfo(t)
	return nil
}

func (b *BackendDS) PersistPod(pod Pod, eventType string) error {
	podEvent := convertPodToPodEvent(pod, eventType)
	b.podEventChan <- &podEvent
	return nil
}

func (b *BackendDS) PersistService(service Service, eventType string) error {
	svcEvent := convertSvcToSvcEvent(service, eventType)
	b.svcEventChan <- &svcEvent
	return nil
}

func (b *BackendDS) PersistDeployment(d Deployment, eventType string) error {
	depEvent := convertDepToDepEvent(d, eventType)
	b.depEventChan <- &depEvent
	return nil
}

func (b *BackendDS) PersistReplicaSet(rs ReplicaSet, eventType string) error {
	rsEvent := convertRsToRsEvent(rs, eventType)
	b.rsEventChan <- &rsEvent
	return nil
}

func (b *BackendDS) PersistEndpoints(ep Endpoints, eventType string) error {
	epEvent := convertEpToEpEvent(ep, eventType)
	b.epEventChan <- &epEvent
	return nil
}

func (b *BackendDS) PersistDaemonSet(ds DaemonSet, eventType string) error {
	dsEvent := convertDsToDsEvent(ds, eventType)
	b.dsEventChan <- &dsEvent
	return nil
}

func (b *BackendDS) PersistContainer(c Container, eventType string) error {
	cEvent := convertContainerToContainerEvent(c, eventType)
	b.containerEventChan <- &cEvent
	return nil
}

func (b *BackendDS) SendHealthCheck(ebpf bool, metrics bool, dist bool, k8sVersion string) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()

	createHealthCheckPayload := func() HealthCheckPayload {
		return HealthCheckPayload{
			Metadata: Metadata{
				MonitoringID:   MonitoringID,
				IdempotencyKey: string(uuid.NewUUID()),
				NodeID:         NodeID,
				AlazVersion:    tag,
			},
			Info: struct {
				EbpfEnabled        bool `json:"ebpf"`
				MetricsEnabled     bool `json:"metrics"`
				DistTracingEnabled bool `json:"traffic"`
			}{
				EbpfEnabled:        ebpf,
				MetricsEnabled:     metrics,
				DistTracingEnabled: dist,
			},
			Telemetry: struct {
				KernelVersion string `json:"kernel_version"`
				K8sVersion    string `json:"k8s_version"`
				CloudProvider string `json:"cloud_provider"`
			}{
				KernelVersion: kernelVersion,
				K8sVersion:    k8sVersion,
				CloudProvider: string(cloudProvider),
			},
		}
	}

	for {
		select {
		case <-b.ctx.Done():
			log.Logger.Info().Msg("stopping sending health check")
			return
		case <-t.C:
			b.sendToBackend(http.MethodPut, createHealthCheckPayload(), healthCheckEndpoint)
		}
	}
}

func (b *BackendDS) exportNodeMetrics() {
	kingpin.Version(version.Print("alaz_node_exporter"))
	kingpin.CommandLine.UsageWriter(os.Stdout)
	kingpin.HelpFlag.Short('h')
	kingpin.Parse() // parse container arguments

	metricsPath := "/inner/metrics"
	h := newHandler(nodeExportLogger{logger: log.Logger})
	http.Handle(metricsPath, h)
	http.ListenAndServe(fmt.Sprintf(":%d", innerMetricsPort), nil)
}

type nodeExporterHandler struct {
	inner  http.Handler
	logger nodeExportLogger
}

func newHandler(logger nodeExportLogger) *nodeExporterHandler {
	h := &nodeExporterHandler{
		logger: logger,
	}

	if innerHandler, err := h.innerHandler(); err != nil {
		log.Logger.Error().Msgf("Couldn't create metrics handler: %s", err)
	} else {
		h.inner = innerHandler
	}
	return h
}

type nodeExportLogger struct {
	logger zerolog.Logger
}

func (l nodeExportLogger) Log(keyvals ...interface{}) error {
	l.logger.Debug().Msg(fmt.Sprint(keyvals...))
	return nil
}

func (h *nodeExporterHandler) innerHandler(filters ...string) (http.Handler, error) {
	nc, err := collector.NewNodeCollector(h.logger, filters...)
	if err != nil {
		return nil, fmt.Errorf("couldn't create collector: %s", err)
	}

	// Only log the creation of an unfiltered handler, which should happen
	// only once upon startup.
	if len(filters) == 0 {
		level.Info(h.logger).Log("msg", "Enabled collectors")
		collectors := []string{}
		for n := range nc.Collectors {
			collectors = append(collectors, n)
		}
		sort.Strings(collectors)
		for _, c := range collectors {
			level.Info(h.logger).Log("collector", c)
		}
	}

	r := prometheus.NewRegistry()
	r.MustRegister(version.NewCollector("alaz_node_exporter"))
	if err := r.Register(nc); err != nil {
		return nil, fmt.Errorf("couldn't register node collector: %s", err)
	}

	handler := promhttp.HandlerFor(
		prometheus.Gatherers{r},
		promhttp.HandlerOpts{},
	)

	return handler, nil
}

func (h *nodeExporterHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	filters := r.URL.Query()["collect[]"]

	if len(filters) == 0 {
		// No filters, use the prepared unfiltered handler.
		h.inner.ServeHTTP(w, r)
		return
	}
	// To serve filtered metrics, we create a filtering handler on the fly.
	filteredHandler, err := h.innerHandler(filters...)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(fmt.Sprintf("Couldn't create filtered metrics handler: %s", err)))
		return
	}
	filteredHandler.ServeHTTP(w, r)
}

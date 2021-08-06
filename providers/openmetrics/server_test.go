// Copyright (c) The go-grpc-middleware Authors.
// Licensed under the Apache License 2.0.

package metrics

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb_testproto "github.com/grpc-ecosystem/go-grpc-middleware/providers/openmetrics/v2/testproto/v1"
)

const (
	pingDefaultValue   = "I like kittens."
	countListResponses = 20
)

func TestServerInterceptorSuite(t *testing.T) {
	suite.Run(t, &ServerInterceptorTestSuite{})
}

type ServerInterceptorTestSuite struct {
	suite.Suite

	serverListener net.Listener
	server         *grpc.Server
	clientConn     *grpc.ClientConn
	testClient     pb_testproto.TestServiceClient
	ctx            context.Context
	cancel         context.CancelFunc

	serverMetrics *ServerMetrics
}

func (s *ServerInterceptorTestSuite) SetupSuite() {
	var err error

	// Make all RPC calls last at most 2 sec, meaning all async issues or deadlock will not kill tests.
	s.ctx, s.cancel = context.WithTimeout(context.TODO(), 2*time.Second)

	s.serverMetrics = NewServerMetrics(WithServerHandlingTimeHistogram())

	s.serverListener, err = net.Listen("tcp", "127.0.0.1:0")
	require.NoError(s.T(), err, "must be able to allocate a port for serverListener")

	// This is the point where we hook up the interceptor
	s.server = grpc.NewServer(
		grpc.StreamInterceptor(StreamServerInterceptor(s.serverMetrics)),
		grpc.UnaryInterceptor(UnaryServerInterceptor(s.serverMetrics)),
	)
	pb_testproto.RegisterTestServiceServer(s.server, &testService{t: s.T()})

	go func() {
		err = s.server.Serve(s.serverListener)
		require.NoError(s.T(), err, "must not error on server listening")
	}()

	s.clientConn, err = grpc.DialContext(s.ctx, s.serverListener.Addr().String(), grpc.WithInsecure(), grpc.WithBlock())
	require.NoError(s.T(), err, "must not error on client Dial")
	s.testClient = pb_testproto.NewTestServiceClient(s.clientConn)
}

func (s *ServerInterceptorTestSuite) SetupTest() {
	// Make all RPC calls last at most 2 sec, meaning all async issues or deadlock will not kill tests.
	s.ctx, s.cancel = context.WithTimeout(context.TODO(), 2*time.Second)

	// Make sure every test starts with same fresh, intialized metric state.
	s.serverMetrics.serverStartedCounter.Reset()
	s.serverMetrics.serverHandledCounter.Reset()
	s.serverMetrics.serverHandledHistogram.Reset()
	s.serverMetrics.serverStreamMsgReceived.Reset()
	s.serverMetrics.serverStreamMsgSent.Reset()
	s.serverMetrics.InitializeMetrics(s.server)
}

func (s *ServerInterceptorTestSuite) TearDownSuite() {
	if s.clientConn != nil {
		s.clientConn.Close()
	}
	if s.serverListener != nil {
		s.server.Stop()
		s.T().Logf("stopped grpc.Server at: %v", s.serverListener.Addr().String())
		s.serverListener.Close()
	}
}

func (s *ServerInterceptorTestSuite) TearDownTest() {
	s.cancel()
}

func (s *ServerInterceptorTestSuite) TestRegisterPresetsStuff() {
	registry := prometheus.NewPedanticRegistry()

	s.Require().NoError(s.serverMetrics.Register(registry))

	for testID, testCase := range []struct {
		metricName     string
		existingLabels []string
	}{
		// Order of label is irrelevant.
		{"grpc_server_started_total", []string{"providers.openmetrics.testproto.v1.TestService", "PingEmpty", "unary"}},
		{"grpc_server_started_total", []string{"providers.openmetrics.testproto.v1.TestService", "PingList", "server_stream"}},
		{"grpc_server_msg_received_total", []string{"providers.openmetrics.testproto.v1.TestService", "PingList", "server_stream"}},
		{"grpc_server_msg_sent_total", []string{"providers.openmetrics.testproto.v1.TestService", "PingEmpty", "unary"}},
		{"grpc_server_handling_seconds_sum", []string{"providers.openmetrics.testproto.v1.TestService", "PingEmpty", "unary"}},
		{"grpc_server_handling_seconds_count", []string{"providers.openmetrics.testproto.v1.TestService", "PingList", "server_stream"}},
		{"grpc_server_handled_total", []string{"providers.openmetrics.testproto.v1.TestService", "PingList", "server_stream", "OutOfRange"}},
		{"grpc_server_handled_total", []string{"providers.openmetrics.testproto.v1.TestService", "PingList", "server_stream", "Aborted"}},
		{"grpc_server_handled_total", []string{"providers.openmetrics.testproto.v1.TestService", "PingEmpty", "unary", "FailedPrecondition"}},
		{"grpc_server_handled_total", []string{"providers.openmetrics.testproto.v1.TestService", "PingEmpty", "unary", "ResourceExhausted"}},
	} {
		lineCount := len(fetchPrometheusLines(s.T(), registry, testCase.metricName, testCase.existingLabels...))
		assert.NotZero(s.T(), lineCount, "metrics must exist for test case %d", testID)
	}
}

func (s *ServerInterceptorTestSuite) TestUnaryIncrementsMetrics() {
	_, err := s.testClient.PingEmpty(s.ctx, &pb_testproto.PingEmptyRequest{}) // should return with code=OK
	require.NoError(s.T(), err)
	requireValue(s.T(), 1, s.serverMetrics.serverStartedCounter.WithLabelValues("unary", "providers.openmetrics.testproto.v1.TestService", "PingEmpty"))
	requireValue(s.T(), 1, s.serverMetrics.serverHandledCounter.WithLabelValues("unary", "providers.openmetrics.testproto.v1.TestService", "PingEmpty", "OK"))
	requireValueHistCount(s.T(), 1, s.serverMetrics.serverHandledHistogram.WithLabelValues("unary", "providers.openmetrics.testproto.v1.TestService", "PingEmpty"))

	_, err = s.testClient.PingError(s.ctx, &pb_testproto.PingErrorRequest{ErrorCodeReturned: uint32(codes.FailedPrecondition)}) // should return with code=FailedPrecondition
	require.Error(s.T(), err)
	requireValue(s.T(), 1, s.serverMetrics.serverStartedCounter.WithLabelValues("unary", "providers.openmetrics.testproto.v1.TestService", "PingError"))
	requireValue(s.T(), 1, s.serverMetrics.serverHandledCounter.WithLabelValues("unary", "providers.openmetrics.testproto.v1.TestService", "PingError", "FailedPrecondition"))
	requireValueHistCount(s.T(), 1, s.serverMetrics.serverHandledHistogram.WithLabelValues("unary", "providers.openmetrics.testproto.v1.TestService", "PingError"))
}

func (s *ServerInterceptorTestSuite) TestStartedStreamingIncrementsStarted() {
	_, err := s.testClient.PingList(s.ctx, &pb_testproto.PingListRequest{})
	require.NoError(s.T(), err)
	requireValueWithRetry(s.ctx, s.T(), 1,
		s.serverMetrics.serverStartedCounter.WithLabelValues("server_stream", "providers.openmetrics.testproto.v1.TestService", "PingList"))

	_, err = s.testClient.PingList(s.ctx, &pb_testproto.PingListRequest{ErrorCodeReturned: uint32(codes.FailedPrecondition)}) // should return with code=FailedPrecondition
	require.NoError(s.T(), err, "PingList must not fail immediately")
	requireValueWithRetry(s.ctx, s.T(), 2,
		s.serverMetrics.serverStartedCounter.WithLabelValues("server_stream", "providers.openmetrics.testproto.v1.TestService", "PingList"))
}

func (s *ServerInterceptorTestSuite) TestStreamingIncrementsMetrics() {
	ss, _ := s.testClient.PingList(s.ctx, &pb_testproto.PingListRequest{}) // should return with code=OK
	// Do a read, just for kicks.
	count := 0
	for {
		_, err := ss.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(s.T(), err, "reading pingList shouldn't fail")
		count++
	}
	require.EqualValues(s.T(), countListResponses, count, "Number of received msg on the wire must match")

	requireValueWithRetry(s.ctx, s.T(), 1,
		s.serverMetrics.serverStartedCounter.WithLabelValues("server_stream", "providers.openmetrics.testproto.v1.TestService", "PingList"))
	requireValueWithRetry(s.ctx, s.T(), 1,
		s.serverMetrics.serverHandledCounter.WithLabelValues("server_stream", "providers.openmetrics.testproto.v1.TestService", "PingList", "OK"))
	requireValueWithRetry(s.ctx, s.T(), countListResponses,
		s.serverMetrics.serverStreamMsgSent.WithLabelValues("server_stream", "providers.openmetrics.testproto.v1.TestService", "PingList"))
	requireValueWithRetry(s.ctx, s.T(), 1,
		s.serverMetrics.serverStreamMsgReceived.WithLabelValues("server_stream", "providers.openmetrics.testproto.v1.TestService", "PingList"))
	requireValueWithRetryHistCount(s.ctx, s.T(), 1,
		s.serverMetrics.serverHandledHistogram.WithLabelValues("server_stream", "providers.openmetrics.testproto.v1.TestService", "PingList"))

	_, err := s.testClient.PingList(s.ctx, &pb_testproto.PingListRequest{ErrorCodeReturned: uint32(codes.FailedPrecondition)}) // should return with code=FailedPrecondition
	require.NoError(s.T(), err, "PingList must not fail immediately")

	requireValueWithRetry(s.ctx, s.T(), 2,
		s.serverMetrics.serverStartedCounter.WithLabelValues("server_stream", "providers.openmetrics.testproto.v1.TestService", "PingList"))
	requireValueWithRetry(s.ctx, s.T(), 1,
		s.serverMetrics.serverHandledCounter.WithLabelValues("server_stream", "providers.openmetrics.testproto.v1.TestService", "PingList", "FailedPrecondition"))
	requireValueWithRetryHistCount(s.ctx, s.T(), 2,
		s.serverMetrics.serverHandledHistogram.WithLabelValues("server_stream", "providers.openmetrics.testproto.v1.TestService", "PingList"))
}

// fetchPrometheusLines does mocked HTTP GET request against real prometheus handler to get the same view that Prometheus
// would have while scraping this endpoint.
// Order of matching label vales does not matter.
func fetchPrometheusLines(t *testing.T, reg prometheus.Gatherer, metricName string, matchingLabelValues ...string) []string {
	resp := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/", nil)
	require.NoError(t, err, "failed creating request for Prometheus handler")

	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(resp, req)
	reader := bufio.NewReader(resp.Body)

	var ret []string
	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		} else {
			require.NoError(t, err, "error reading stuff")
		}
		if !strings.HasPrefix(line, metricName) {
			continue
		}
		matches := true
		for _, labelValue := range matchingLabelValues {
			if !strings.Contains(line, `"`+labelValue+`"`) {
				matches = false
			}
		}
		if matches {
			ret = append(ret, line)
		}

	}
	return ret
}

type testService struct {
	t *testing.T
}

func (s *testService) PingEmpty(ctx context.Context, _ *pb_testproto.PingEmptyRequest) (*pb_testproto.PingEmptyResponse, error) {
	return &pb_testproto.PingEmptyResponse{Value: pingDefaultValue, Counter: 42}, nil
}

func (s *testService) Ping(ctx context.Context, ping *pb_testproto.PingRequest) (*pb_testproto.PingResponse, error) {
	// Send user trailers and headers.
	if _, ok := grpc.Method(ctx); !ok {
		return nil, errors.New("cannot retrieve method name")
	}
	return &pb_testproto.PingResponse{Value: ping.Value, Counter: 42}, nil
}

func (s *testService) PingError(ctx context.Context, ping *pb_testproto.PingErrorRequest) (*pb_testproto.PingErrorResponse, error) {
	code := codes.Code(ping.ErrorCodeReturned)
	return nil, status.Error(code, "Userspace error.")
}

func (s *testService) PingList(ping *pb_testproto.PingListRequest, stream pb_testproto.TestService_PingListServer) error {
	if _, ok := grpc.Method(stream.Context()); !ok {
		return errors.New("cannot retrieve method name")
	}
	if ping.ErrorCodeReturned != 0 {
		return status.Error(codes.Code(ping.ErrorCodeReturned), "foobar")
	}
	// Send user trailers and headers.
	for i := 0; i < countListResponses; i++ {
		err := stream.Send(&pb_testproto.PingListResponse{Value: ping.Value, Counter: int32(i)})
		require.NoError(s.t, err, "must not error on server sending streaming values")
	}
	return nil
}

// toFloat64HistCount does the same thing as prometheus go client testutil.ToFloat64, but for histograms.
// TODO(bwplotka): Upstream this function to prometheus client.
func toFloat64HistCount(h prometheus.Observer) uint64 {
	var (
		m      prometheus.Metric
		mCount int
		mChan  = make(chan prometheus.Metric)
		done   = make(chan struct{})
	)

	go func() {
		for m = range mChan {
			mCount++
		}
		close(done)
	}()

	c, ok := h.(prometheus.Collector)
	if !ok {
		panic(fmt.Errorf("observer is not a collector; got: %T", h))
	}

	c.Collect(mChan)
	close(mChan)
	<-done

	if mCount != 1 {
		panic(fmt.Errorf("collected %d metrics instead of exactly 1", mCount))
	}

	pb := &dto.Metric{}
	if err := m.Write(pb); err != nil {
		panic(fmt.Errorf("metric write failed, err=%v", err))
	}

	if pb.Histogram != nil {
		return pb.Histogram.GetSampleCount()
	}
	panic(fmt.Errorf("collected a non-histogram metric: %s", pb))
}

func requireValue(t *testing.T, expect int, c prometheus.Collector) {
	t.Helper()
	v := int(testutil.ToFloat64(c))
	if v == expect {
		return
	}

	metricFullName := reflect.ValueOf(*c.(prometheus.Metric).Desc()).FieldByName("fqName").String()
	t.Errorf("expected %d %s value; got %d; ", expect, metricFullName, v)
	t.Fail()
}

func requireValueHistCount(t *testing.T, expect int, o prometheus.Observer) {
	t.Helper()
	v := int(toFloat64HistCount(o))
	if v == expect {
		return
	}

	metricFullName := reflect.ValueOf(*o.(prometheus.Metric).Desc()).FieldByName("fqName").String()
	t.Errorf("expected %d %s value; got %d; ", expect, metricFullName, v)
	t.Fail()
}

func requireValueWithRetry(ctx context.Context, t *testing.T, expect int, c prometheus.Collector) {
	t.Helper()
	for {
		v := int(testutil.ToFloat64(c))
		if v == expect {
			return
		}

		select {
		case <-ctx.Done():
			metricFullName := reflect.ValueOf(*c.(prometheus.Metric).Desc()).FieldByName("fqName").String()
			t.Errorf("timeout while expecting %d %s value; got %d; ", expect, metricFullName, v)
			t.Fail()
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func requireValueWithRetryHistCount(ctx context.Context, t *testing.T, expect int, o prometheus.Observer) {
	t.Helper()
	for {
		v := int(toFloat64HistCount(o))
		if v == expect {
			return
		}

		select {
		case <-ctx.Done():
			metricFullName := reflect.ValueOf(*o.(prometheus.Metric).Desc()).FieldByName("fqName").String()
			t.Errorf("timeout while expecting %d %s histogram count value; got %d; ", expect, metricFullName, v)
			t.Fail()
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}
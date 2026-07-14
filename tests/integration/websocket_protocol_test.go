package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/veqri/veqri/tests/integration/testfixture"
)

func TestAuthenticatedWebSocketsRequireV1Subprotocol(t *testing.T) {
	fixture := testfixture.New(t, testfixture.Options{NoWorkers: true})
	device := fixture.PairAndroid(t, "WebSocket protocol phone")
	streams := []struct {
		name   string
		path   string
		header http.Header
	}{
		{
			name: "raw admin", path: "/v1/stream",
			header: http.Header{"Authorization": []string{"Bearer " + fixture.AdminToken}},
		},
		{
			name: "desktop", path: "/api/v1/events",
			header: http.Header{"Authorization": []string{"Bearer " + fixture.AdminToken}},
		},
		{
			name: "device", path: "/v1/device/events",
			header: http.Header{
				"Authorization":            []string{"Bearer " + device.Credential},
				"X-Veqri-Device-Id":        []string{device.ID},
				"X-Veqri-Protocol-Version": []string{"1"},
			},
		},
	}
	protocols := []struct {
		name   string
		offers []string
	}{
		{name: "missing"},
		{name: "unknown", offers: []string{"veqri.v2"}},
	}

	for _, stream := range streams {
		for _, protocol := range protocols {
			t.Run(stream.name+"/"+protocol.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				connection, response, err := websocket.Dial(ctx, fixture.WebSocketURL(stream.path),
					&websocket.DialOptions{HTTPHeader: stream.header.Clone(), Subprotocols: protocol.offers})
				if connection != nil {
					connection.CloseNow()
				}
				if response != nil {
					defer response.Body.Close()
				}
				if err == nil || response == nil || response.StatusCode != http.StatusUpgradeRequired {
					t.Fatalf("result = conn:%v response:%v err:%v, want HTTP 426",
						connection != nil, response, err)
				}
			})
		}
	}
}

func TestWebSocketProtocolMayBeOfferedInARepeatedHeader(t *testing.T) {
	fixture := testfixture.New(t, testfixture.Options{NoWorkers: true})
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		request.Header.Del("Sec-WebSocket-Protocol")
		request.Header.Add("Sec-WebSocket-Protocol", "veqri.auth.placeholder")
		request.Header.Add("Sec-WebSocket-Protocol", "veqri.v1")
		return http.DefaultTransport.RoundTrip(request)
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connection, response, err := websocket.Dial(ctx, fixture.WebSocketURL("/v1/stream"),
		&websocket.DialOptions{
			HTTPClient:   client,
			HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + fixture.AdminToken}},
			Subprotocols: []string{"veqri.auth.placeholder", "veqri.v1"},
		})
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial with veqri.v1 in repeated header: %v", err)
	}
	defer connection.CloseNow()
	if connection.Subprotocol() != "veqri.v1" {
		t.Fatalf("negotiated subprotocol = %q, want veqri.v1", connection.Subprotocol())
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

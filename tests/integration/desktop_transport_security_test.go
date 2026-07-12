package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/veqri/veqri/tests/integration/testfixture"
)

func TestPackagedDesktopOriginPreflightAndAdminOnlyRawStream(t *testing.T) {
	fixture := testfixture.New(t, testfixture.Options{WorkerCount: 1})
	request, err := http.NewRequest(http.MethodOptions, fixture.HTTP.URL+"/api/v1/desktop/snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", "wails://wails")
	request.Header.Set("Access-Control-Request-Method", http.MethodGet)
	request.Header.Set("Access-Control-Request-Headers", "authorization,x-veqri-client")
	response, err := fixture.HTTP.Client().Do(request)
	if err != nil {
		t.Fatalf("desktop preflight: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", response.StatusCode)
	}
	if response.Header.Get("Access-Control-Allow-Origin") != "wails://wails" ||
		!strings.Contains(strings.ToLower(response.Header.Get("Access-Control-Allow-Headers")), "x-veqri-client") {
		t.Fatalf("packaged origin preflight headers = %v", response.Header)
	}

	ctx := context.Background()
	adminConnection, adminResponse, err := websocket.Dial(ctx, fixture.WebSocketURL("/api/v1/events"), &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + fixture.AdminToken},
			"Origin":        []string{"wails://wails"},
		},
		Subprotocols: []string{"veqri.v1"},
	})
	if err != nil {
		if adminResponse != nil {
			t.Fatalf("packaged desktop websocket: %v (HTTP %s)", err, adminResponse.Status)
		}
		t.Fatalf("packaged desktop websocket: %v", err)
	}
	adminConnection.CloseNow()

	device := fixture.PairAndroid(t, "Raw stream isolation phone")
	deviceConnection, deviceResponse, err := websocket.Dial(ctx, fixture.WebSocketURL("/v1/stream"), &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + device.Credential}},
		Subprotocols: []string{"veqri.v1"},
	})
	if deviceConnection != nil {
		deviceConnection.CloseNow()
	}
	if err == nil || deviceResponse == nil || deviceResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("device raw stream result = conn:%v response:%v err:%v, want HTTP 401", deviceConnection != nil, deviceResponse, err)
	}
}

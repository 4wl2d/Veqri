package integration_test

import (
	"net/http"
	"testing"

	"github.com/veqri/veqri/internal/buildinfo"
	"github.com/veqri/veqri/tests/integration/testfixture"
)

func TestBuildInfoIsIdenticalAcrossRuntimeSurfaces(t *testing.T) {
	fixture := testfixture.New(t, testfixture.Options{NoWorkers: true})
	want := buildinfo.Development()

	for _, path := range []string{"/healthz", "/readyz"} {
		response := fixture.JSON(t, http.MethodGet, path, "", nil, nil)
		testfixture.RequireStatus(t, response, http.StatusOK)
		payload := testfixture.Decode[struct {
			buildinfo.Info
		}](t, response)
		if payload.Info != want {
			t.Errorf("%s build info = %+v, want %+v", path, payload.Info, want)
		}
	}

	response := fixture.JSON(t, http.MethodGet, "/api/v1/desktop/snapshot", fixture.AdminToken, nil, nil)
	testfixture.RequireStatus(t, response, http.StatusOK)
	snapshot := testfixture.Decode[struct {
		Core buildinfo.Info `json:"core"`
	}](t, response)
	if snapshot.Core != want {
		t.Fatalf("desktop snapshot build info = %+v, want %+v", snapshot.Core, want)
	}
}

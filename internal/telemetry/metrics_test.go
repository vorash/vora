package telemetry

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vorash/vora/internal/version"
)

func TestMetricsExposeBuildVersion(t *testing.T) {
	previous := version.Version
	version.Version = "v-test"
	t.Cleanup(func() { version.Version = previous })

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/metrics", nil)
	New().Handler().ServeHTTP(recorder, request)

	response := recorder.Result()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read metrics response: %v", err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("metrics status = %d, want 200", response.StatusCode)
	}
	if !strings.Contains(string(body), `vora_build_info{version="v-test"} 1`) {
		t.Fatalf("metrics do not identify the running build:\n%s", body)
	}
}

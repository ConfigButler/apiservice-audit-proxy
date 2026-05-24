package proxy

import (
	"fmt"
	"os"
	"testing"

	"github.com/ConfigButler/apiservice-audit-proxy/pkg/telemetry"
)

// TestMain wires telemetry instruments to a manual-reader meter provider once
// for the whole package. Without it, handler tests that exercise the request
// path would panic on the first call to a nil instrument, since the production
// init now happens explicitly in cmd/server.
func TestMain(m *testing.M) {
	if _, err := telemetry.InitTestExporter(); err != nil {
		fmt.Fprintf(os.Stderr, "init telemetry test exporter: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

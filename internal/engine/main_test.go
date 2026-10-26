package engine

import (
	"flag"
	"os"
	"testing"
)

// TestMain bounds parallel tests unless -parallel is given explicitly: each
// integration harness holds its own PostgreSQL pool, and one test per CPU
// would exhaust the shared server's connection limit. Tests that drive the
// live event loop against real-time gates stay serial, so they run before the
// parallel tests and never compete with them.
func TestMain(m *testing.M) {
	flag.Parse()
	explicit := false
	flag.Visit(func(f *flag.Flag) {
		explicit = explicit || f.Name == "test.parallel"
	})
	if !explicit {
		if err := flag.Set("test.parallel", "6"); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}

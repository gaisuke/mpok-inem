package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestMain runs the suite and then refuses to pass if any registered route was
// never exercised: "every endpoint covered" is enforced, not just claimed. Add
// an endpoint to allRoutes() without a test and this test fails with its name.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 && !noDB {
		var missing []string
		for _, rt := range allRoutes() {
			if !hits[rt.Method+" "+rt.Pattern] {
				missing = append(missing, rt.Method+" "+rt.Pattern)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			fmt.Fprintf(os.Stderr, "\nroute coverage gate: %d endpoint(s) never exercised:\n  %s\n",
				len(missing), strings.Join(missing, "\n  "))
			code = 1
		} else {
			fmt.Printf("route coverage gate: all %d endpoints exercised\n", len(allRoutes()))
		}
	}
	os.Exit(code)
}

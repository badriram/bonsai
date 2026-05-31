// Package progress prints phase-level updates to stderr during long-running
// operations like grow. Stays out of stdout so the final endpoint lines from
// cli/grow.go remain machine-parseable. Always-on for now; --quiet/--verbose
// will gate this once the broader config refactor lands.
package progress

import (
	"fmt"
	"os"
	"time"
)

var start = time.Now()

func Step(format string, args ...any) {
	elapsed := time.Since(start).Round(time.Second)
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "[%s] %s\n", elapsed, msg)
}

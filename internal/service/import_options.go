package service

import (
	"fmt"
	"strings"

	"srv/internal/provision"
)

// parseImportOptions accepts the full import command argv.
func parseImportOptions(args []string) (provision.ImportOptions, error) {
	var opts provision.ImportOptions
	if len(args) == 0 || args[0] != "import" {
		return opts, fmt.Errorf("%s", importUsage())
	}
	seen := false
	for i := 1; i < len(args); i++ {
		if seen {
			return opts, fmt.Errorf("memory policy may only be specified once")
		}
		seen = true
		switch args[i] {
		case "--fixed-memory":
			opts.FixedMemory = true
		case "--pool":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" || strings.HasPrefix(args[i], "--") {
				return opts, fmt.Errorf("--pool requires a target pool name")
			}
			opts.MemoryPoolName = args[i]
		default:
			return opts, fmt.Errorf("%s: unknown option %q", importUsage(), args[i])
		}
	}
	return opts, opts.Validate(false)
}

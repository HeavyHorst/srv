// srv-cold-store is installed as a root SSH forced command, for example:
// restrict,command="/usr/local/sbin/srv-cold-store --root /storage/host" ssh-ed25519 ...
// The configured peer and its btrfs send streams MUST be trusted.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"srv/internal/coldstore"
)

func main() {
	root := flag.String("root", "", "administrator-configured absolute btrfs storage root")
	timeout := flag.Duration("timeout", coldstore.DefaultTimeout, "maximum operation duration (must cover transfer and checksum verification)")
	discard := flag.String("discard", "", "LOCAL ADMIN ONLY: irreversibly discard comma-separated object UUIDs, including corrupt manifests and their dependents")
	flag.Parse()
	if flag.NArg() != 0 || *root == "" || *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "--root and a positive --timeout are required; positional arguments are forbidden")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	var err error
	if *discard != "" {
		if _, ssh := os.LookupEnv("SSH_ORIGINAL_COMMAND"); ssh {
			fmt.Fprintln(os.Stderr, "--discard is forbidden in forced commands")
			os.Exit(2)
		}
		err = coldstore.DiscardObjects(ctx, *root, strings.Split(*discard, ","))
	} else {
		err = coldstore.Serve(ctx, *root, os.Getenv("SSH_ORIGINAL_COMMAND"), os.Stdin, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

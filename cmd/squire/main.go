package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"squire.run/kernel/internal/kernel"
)

func main() {
	ctx := context.Background()
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	storeRoot := kernel.DefaultStoreRoot(cwd)
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	var out string
	switch {
	case len(args) == 1 && args[0] == "setup":
		out, err = kernel.Setup(ctx, cwd, storeRoot)
	case len(args) == 2 && args[0] == "kernel" && args[1] == "status":
		out, err = kernel.KernelStatus(ctx, cwd, storeRoot)
	case len(args) == 2 && args[0] == "boost" && args[1] == "status":
		out, err = kernel.BoostStatus(ctx, cwd, storeRoot)
	case len(args) == 2 && args[0] == "shadow" && args[1] == "status":
		out, err = kernel.ShadowStatus(ctx, cwd, storeRoot)
	case len(args) == 3 && args[0] == "boost" && args[1] == "bench" && args[2] == "repo-metadata":
		var report kernel.BenchReport
		report, err = kernel.BenchRepoMetadata(ctx)
		if err == nil {
			b, _ := json.MarshalIndent(report, "", "  ")
			out = string(b) + "\n"
		}
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(out)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  squire setup")
	fmt.Fprintln(os.Stderr, "  squire kernel status")
	fmt.Fprintln(os.Stderr, "  squire boost status")
	fmt.Fprintln(os.Stderr, "  squire shadow status")
	fmt.Fprintln(os.Stderr, "  squire boost bench repo-metadata")
}

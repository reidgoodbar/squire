//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "squire-vm-agent runs inside the Linux guest only")
	os.Exit(1)
}

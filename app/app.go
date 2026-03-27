package app

import (
	"io"

	internalapp "squire/internal/app"
)

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return internalapp.Run(args, stdin, stdout, stderr)
}

package main

import (
	"fmt"
	"os"

	"github.com/Bunn-Burrow/bunn-cli/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}

// Ensure fmt is used (for potential future error formatting).
var _ = fmt.Sprintf

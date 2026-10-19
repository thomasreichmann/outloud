// Command mkicon writes Outloud's application icon as an .ico file, for
// embedding into the Windows executable.
package main

import (
	"fmt"
	"os"

	"github.com/thomasreichmann/outloud/internal/icon"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mkicon out.ico")
		os.Exit(2)
	}
	ico := icon.ICO(icon.App, 16, 20, 24, 32, 40, 48, 64, 256)
	if err := os.WriteFile(os.Args[1], ico, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

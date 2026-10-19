package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/thomasreichmann/outloud/internal/pairing"
)

// dataDir holds this receiver's key and its paired PCs. On a Mac that is
// ~/Library/Application Support/Outloud, where the app is installed too.
func dataDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "Outloud")
}

func listPaired() error {
	peers, err := pairing.OpenStore(dataDir()).List()
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		fmt.Println("No PC is paired with this computer.")
		return nil
	}
	for _, p := range peers {
		fmt.Printf("%-32s paired %s, key %s\n", p.Name, p.Paired.Local().Format("2006-01-02 15:04"), pairing.Short(p.ID))
	}
	return nil
}

// forget unpairs the PCs that args[0] names, by name or key, or all of
// them. A running receiver drops them within seconds.
func forget(args []string) error {
	if len(args) != 1 {
		return errors.New(`say which PC to forget, by name or key as "outloud-receiver paired" lists them, or "all"`)
	}
	gone, err := pairing.OpenStore(dataDir()).Forget(args[0])
	if err != nil {
		return err
	}
	for _, p := range gone {
		fmt.Printf("Forgot %s. It has to pair again to connect.\n", p.Name)
	}
	if len(gone) == 0 && args[0] != "all" {
		return fmt.Errorf("no paired PC is called %q; see \"outloud-receiver paired\"", args[0])
	}
	return nil
}

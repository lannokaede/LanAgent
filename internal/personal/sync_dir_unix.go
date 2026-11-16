//go:build !windows

package personal

import (
	"errors"
	"os"
)

func syncSessionDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

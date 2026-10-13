//go:build !windows

package billingledger

import "os"

func persistPendingFile(from, to, directory string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	return syncRemovedEntry(directory)
}

func syncRemovedEntry(directory string) error {
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

package billingledger

import "golang.org/x/sys/windows"

// Windows does not support POSIX directory fsync. The temporary file has
// already been flushed; publish it using a same-volume write-through move.
func persistPendingFile(from, to, _ string) error {
	source, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(source, target, windows.MOVEFILE_WRITE_THROUGH|windows.MOVEFILE_REPLACE_EXISTING)
}

// A removal lost on restart only replays the same durable Redis event ID.
// Expired receipts still fail closed instead of charging again.
func syncRemovedEntry(string) error { return nil }

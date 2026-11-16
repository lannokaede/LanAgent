package personal

import "os"

// Windows does not expose Unix fsync(directory) through os.File.Sync. Retain
// the existing replacement and file-data flushing without failing every checkpoint;
// directory-entry persistence remains subject to the host filesystem there.
func syncSessionDirectory(*os.Root) error { return nil }

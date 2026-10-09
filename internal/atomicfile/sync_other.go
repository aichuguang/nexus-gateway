//go:build !linux

package atomicfile

// Windows does not support os.File.Sync on directory handles. Preserve the
// existing file-sync/rename behaviour rather than failing every replacement.
func syncDirectory(string) error { return nil }

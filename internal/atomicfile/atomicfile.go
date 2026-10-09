// Package atomicfile replaces managed files without exposing partial contents.
// On Linux it syncs both the file and its parent directory. On Windows it keeps
// os.Rename's existing replacement semantics; directory sync is not supported.
package atomicfile

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// Options preserves the difference between application state (which creates its
// private parent directories) and proxy configuration (which requires Prepare).
type Options struct {
	CreateParents bool
}

// CommittedError means the destination already contains the replacement, but
// syncing the parent directory failed. It is NOT evidence that nothing changed.
// Callers must retain the committed state, or perform an explicit checked
// recovery; blindly restoring only their in-memory state would split the ledger.
type CommittedError struct {
	Path string
	Err  error
}

func (e *CommittedError) Error() string {
	return fmt.Sprintf("replacement of %s is committed, but parent-directory durability is unconfirmed: %v", e.Path, e.Err)
}

func (e *CommittedError) Unwrap() error { return e.Err }

// IsCommitted also recognises a CommittedError wrapped by a caller.
func IsCommitted(err error) bool {
	var committed *CommittedError
	return errors.As(err, &committed)
}

// Write creates a 0600 temporary file beside path, writes and syncs its contents,
// closes it, then renames it into place and syncs the parent directory on Linux.
// Before rename, an error leaves an existing destination untouched. After
// rename, a directory-sync failure is reported distinctly as CommittedError.
func Write(path string, data []byte, options Options) error {
	return write(path, data, options, (*os.File).Sync, syncDirectory)
}

// WriteCompatible is for existing callers whose error handling assumes an error
// means the destination did not change. A committed replacement remains success
// for those callers, with an explicit warning that crash durability is unknown.
// New transactional callers should use Write and inspect IsCommitted instead.
func WriteCompatible(path string, data []byte, options Options) error {
	return compatibleResult(Write(path, data, options))
}

func compatibleResult(err error) error {
	if IsCommitted(err) {
		log.Printf("WARNING: %v; retaining the committed file and in-memory state", err)
		return nil
	}
	return err
}

func write(path string, data []byte, options Options, syncFile func(*os.File) error, syncParent func(string) error) error {
	parent := filepath.Dir(path)
	if options.CreateParents {
		if err := os.MkdirAll(parent, 0700); err != nil {
			return err
		}
	}
	f, err := os.CreateTemp(parent, ".write-*")
	if err != nil {
		return err
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = syncFile(f)
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temporary, path); err != nil {
		return err
	}
	if err = syncParent(parent); err != nil {
		return &CommittedError{Path: path, Err: err}
	}
	return nil
}

// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package util

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

const windowsSharingViolationError syscall.Errno = 32

// Remove removes the named file or (empty) directory with at most 5 attempts.
func Remove(name string) error {
	var err error
	for i := 0; i < 5; i++ {
		err = os.Remove(name)
		if err == nil {
			break
		}
		unwrapped := err.(*os.PathError).Err
		if unwrapped == syscall.EBUSY || unwrapped == syscall.ENOTEMPTY || unwrapped == syscall.EPERM || unwrapped == syscall.EMFILE || unwrapped == syscall.ENFILE {
			// try again
			<-time.After(100 * time.Millisecond)
			continue
		}

		if unwrapped == windowsSharingViolationError && runtime.GOOS == "windows" {
			// try again
			<-time.After(100 * time.Millisecond)
			continue
		}

		if unwrapped == syscall.ENOENT {
			// it's already gone
			return nil
		}
	}
	return err
}

// RemoveAll removes the named file or directory with at most 5 attempts.
func RemoveAll(name string) error {
	var err error

	for i := 0; i < 5; i++ {
		// Do chmod -R +w to help ensure the removal succeeds.
		// In particular, in the git-annex case, this handles
		// https://git-annex.branchable.com/internals/lockdown/ :
		//
		// > (The only bad consequence of this is that rm -rf .git
		// > doesn't work unless you first run chmod -R +w .git)

		err = filepath.WalkDir(name, func(path string, d fs.DirEntry, err error) error {
			// NB: this is called WalkDir but it works on a single file too
			if err == nil {
				info, err := d.Info()
				if err != nil {
					return err
				}

				// 0200 == u+w, in octal unix permission notation
				err = os.Chmod(path, info.Mode()|0o200)
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			// try again
			<-time.After(100 * time.Millisecond)
			continue
		}

		err = os.RemoveAll(name)
		if err == nil {
			break
		}
		unwrapped := err.(*os.PathError).Err
		if unwrapped == syscall.EBUSY || unwrapped == syscall.ENOTEMPTY || unwrapped == syscall.EPERM || unwrapped == syscall.EMFILE || unwrapped == syscall.ENFILE {
			// try again
			<-time.After(100 * time.Millisecond)
			continue
		}

		if unwrapped == windowsSharingViolationError && runtime.GOOS == "windows" {
			// try again
			<-time.After(100 * time.Millisecond)
			continue
		}

		if unwrapped == syscall.ENOENT {
			// it's already gone
			return nil
		}
	}
	return err
}

// Rename renames (moves) oldpath to newpath with at most 5 attempts.
func Rename(oldpath, newpath string) error {
	var err error
	for i := 0; i < 5; i++ {
		err = os.Rename(oldpath, newpath)
		if err == nil {
			break
		}
		unwrapped := err.(*os.LinkError).Err
		if unwrapped == syscall.EBUSY || unwrapped == syscall.ENOTEMPTY || unwrapped == syscall.EPERM || unwrapped == syscall.EMFILE || unwrapped == syscall.ENFILE {
			// try again
			<-time.After(100 * time.Millisecond)
			continue
		}

		if unwrapped == windowsSharingViolationError && runtime.GOOS == "windows" {
			// try again
			<-time.After(100 * time.Millisecond)
			continue
		}

		if i == 0 && os.IsNotExist(err) {
			return err
		}

		if unwrapped == syscall.ENOENT {
			// it's already gone
			return nil
		}
	}
	return err
}

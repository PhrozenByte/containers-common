//go:build linux

// Copyright (C) 2024 SUSE LLC. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package securejoin

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

type symlinkStackEntry struct {
	// (dir, remainingPath) is what we would've returned if the link didn't
	// exist. This matches what openat2(RESOLVE_IN_ROOT) would return in
	// this case.
	dir           *os.File
	remainingPath string
	// linkUnwalked is the remaining path components from the original
	// Readlink which we have yet to walk. When this slice is empty, we
	// drop the link from the stack.
	linkUnwalked []string
}

func (se symlinkStackEntry) String() string {
	return fmt.Sprintf("<%s>/%s [->%s]", se.dir.Name(), se.remainingPath, strings.Join(se.linkUnwalked, "/"))
}

func (se symlinkStackEntry) Close() {
	_ = se.dir.Close()
}

type symlinkStack []*symlinkStackEntry

func (s symlinkStack) IsEmpty() bool {
	return len(s) == 0
}

func (s *symlinkStack) Close() {
	for _, link := range *s {
		link.Close()
	}
	// TODO: Switch to clear once we switch to Go 1.21.
	*s = nil
}

var (
	errEmptyStack         = errors.New("[internal] stack is empty")
	errBrokenSymlinkStack = errors.New("[internal error] broken symlink stack")
)

func (s *symlinkStack) popPart(part string) error {
	if s.IsEmpty() {
		// If there is nothing in the symlink stack, then the part was from the
		// real path provided by the user, and this is a no-op.
		return errEmptyStack
	}
	tailEntry := (*s)[len(*s)-1]

	// Double-check that we are popping the component we expect.
	if len(tailEntry.linkUnwalked) == 0 {
		return fmt.Errorf("%w: trying to pop component %q of empty stack entry %s", errBrokenSymlinkStack, part, tailEntry)
	}
	headPart := tailEntry.linkUnwalked[0]
	if headPart != part {
		return fmt.Errorf("%w: trying to pop component %q but the last stack entry is %s (%q)", errBrokenSymlinkStack, part, tailEntry, headPart)
	}

	// Drop the component, but keep the entry around in case we are dealing
	// with a "tail-chained" symlink.
	tailEntry.linkUnwalked = tailEntry.linkUnwalked[1:]
	return nil
}

func (s *symlinkStack) PopPart(part string) error {
	if err := s.popPart(part); err != nil {
		if errors.Is(err, errEmptyStack) {
			// Skip empty stacks.
			err = nil
		}
		return err
	}

	// Clean up any of the trailing stack entries that are empty.
	for lastGood := len(*s) - 1; lastGood >= 0; lastGood-- {
		entry := (*s)[lastGood]
		if len(entry.linkUnwalked) > 0 {
			break
		}
		entry.Close()
		(*s) = (*s)[:lastGood]
	}
	return nil
}

func (s *symlinkStack) push(dir *os.File, remainingPath, linkTarget string) error {
	// Split the link target and clean up any "" parts.
	linkTargetParts := slices.DeleteFunc(
		strings.Split(linkTarget, "/"),
		func(part string) bool { return part == "" })

	// Don't add a no-op link to the stack. You can't create a no-op link
	// symlink, but if the symlink is /, partialLookupInRoot has already jumped to the
	// root and so there's nothing more to do.
	if len(linkTargetParts) == 0 {
		return nil
	}

	// Copy the directory so the caller doesn't close our copy.
	dirCopy, err := dupFile(dir)
	if err != nil {
		return err
	}

	// Add to the stack.
	*s = append(*s, &symlinkStackEntry{
		dir:           dirCopy,
		remainingPath: remainingPath,
		linkUnwalked:  linkTargetParts,
	})
	return nil
}

func (s *symlinkStack) SwapLink(linkPart string, dir *os.File, remainingPath, linkTarget string) error {
	// If we are currently inside a symlink resolution, remove the symlink
	// component from the last symlink entry, but don't remove the entry even
	// if it's empty. If we are a "tail-chained" symlink (a trailing symlink we
	// hit during a symlink resolution) we need to keep the old symlink until
	// we finish the resolution.
	if err := s.popPart(linkPart); err != nil {
		if !errors.Is(err, errEmptyStack) {
			return err
		}
		// Push the component regardless of whether the stack was empty.
	}
	return s.push(dir, remainingPath, linkTarget)
}

func (s *symlinkStack) PopTopSymlink() (*os.File, string, bool) {
	if s.IsEmpty() {
		return nil, "", false
	}
	tailEntry := (*s)[0]
	*s = (*s)[1:]
	return tailEntry.dir, tailEntry.remainingPath, true
}

// partialLookupInRoot tries to lookup as much of the request path as possible
// within the provided root (a-la RESOLVE_IN_ROOT) and opens the final existing
// component of the requested path, returning a file handle to the final
// existing component and a string containing the remaining path components.
func partialLookupInRoot(root *os.File, unsafePath string) (_ *os.File, _ string, Err error) {
	unsafePath = filepath.ToSlash(unsafePath) // noop

	// This is very similar to SecureJoin, except that we operate on the
	// components using file descriptors. We then return the last component we
	// managed open, along with the remaining path components not opened.

	// Try to use openat2 if possible.
	if hasOpenat2() {
		return partialLookupOpenat2(root, unsafePath)
	}

	// Get the "actual" root path from /proc/self/fd. This is necessary if the
	// root is some magic-link like /proc/$pid/root, in which case we want to
	// make sure when we do checkProcSelfFdPath that we are using the correct
	// root path.
	logicalRootPath, err := procSelfFdReadlink(root)
	if err != nil {
		return nil, "", fmt.Errorf("get real root path: %w", err)
	}

	currentDir, err := dupFile(root)
	if err != nil {
		return nil, "", fmt.Errorf("clone root fd: %w", err)
	}
	defer func() {
		if Err != nil {
			_ = currentDir.Close()
		}
	}()

	// symlinkStack is used to emulate how openat2(RESOLVE_IN_ROOT) treats
	// dangling symlinks. If we hit a non-existent path while resolving a
	// symlink, we need to return the (dir, remainingPath) that we had when we
	// hit the symlink (treating the symlink as though it were a regular file).
	// The set of (dir, remainingPath) sets is stored within the symlinkStack
	// and we add and remove parts when we hit symlink and non-symlink
	// components respectively. We need a stack because of recursive symlinks
	// (symlinks that contain symlink components in their target).
	//
	// Note that the stack is ONLY used for book-keeping. All of the actual
	// path walking logic is still based on currentPath/remainingPath and
	// currentDir (as in SecureJoin).
	var symlinkStack symlinkStack
	defer symlinkStack.Close()

	var (
		linksWalked   int
		currentPath   string
		remainingPath = unsafePath
	)
	for remainingPath != "" {
		// Save the current remaining path so if the part is not real we can
		// return the path including the component.
		oldRemainingPath := remainingPath

		// Get the next path component.
		var part string
		if i := strings.IndexByte(remainingPath, '/'); i == -1 {
			part, remainingPath = remainingPath, ""
		} else {
			part, remainingPath = remainingPath[:i], remainingPath[i+1:]
		}
		// Skip any "//" components.
		if part == "" {
			continue
		}

		// Apply the component lexically to the path we are building.
		// currentPath does not contain any symlinks, and we are lexically
		// dealing with a single component, so it's okay to do a filepath.Clean
		// here.
		nextPath := path.Join("/", currentPath, part)
		// If we logically hit the root, just clone the root rather than
		// opening the part and doing all of the other checks.
		if nextPath == "/" {
			if err := symlinkStack.PopPart(part); err != nil {
				return nil, "", fmt.Errorf("walking into root with part %q failed: %w", part, err)
			}
			// Jump to root.
			rootClone, err := dupFile(root)
			if err != nil {
				return nil, "", fmt.Errorf("clone root fd: %w", err)
			}
			_ = currentDir.Close()
			currentDir = rootClone
			currentPath = nextPath
			continue
		}

		// Try to open the next component.
		nextDir, err := openatFile(currentDir, part, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		switch {
		case err == nil:
			st, err := nextDir.Stat()
			if err != nil {
				_ = nextDir.Close()
				return nil, "", fmt.Errorf("stat component %q: %w", part, err)
			}

			switch st.Mode() & os.ModeType {
			case os.ModeDir:
				// If we are dealing with a directory, simply walk into it.
				_ = currentDir.Close()
				currentDir = nextDir
				currentPath = nextPath

				// The part was real, so drop it from the symlink stack.
				if err := symlinkStack.PopPart(part); err != nil {
					return nil, "", fmt.Errorf("walking into directory %q failed: %w", part, err)
				}

				// If we are operating on a .., make sure we haven't escaped.
				// We only have to check for ".." here because walking down
				// into a regular component component cannot cause you to
				// escape. This mirrors the logic in RESOLVE_IN_ROOT, except we
				// have to check every ".." rather than only checking after a
				// rename or mount on the system.
				if part == ".." {
					// Make sure the root hasn't moved.
					if err := checkProcSelfFdPath(logicalRootPath, root); err != nil {
						return nil, "", fmt.Errorf("root path moved during lookup: %w", err)
					}
					// Make sure the path is what we expect.
					fullPath := logicalRootPath + nextPath
					if err := checkProcSelfFdPath(fullPath, currentDir); err != nil {
						return nil, "", fmt.Errorf("walking into %q had unexpected result: %w", part, err)
					}
				}

			case os.ModeSymlink:
				// We don't need the handle anymore.
				_ = nextDir.Close()

				// Unfortunately, we cannot readlink through our handle and so
				// we need to do a separate readlinkat (which could race to
				// give us an error if the attacker swapped the symlink with a
				// non-symlink).
				linkDest, err := readlinkatFile(currentDir, part)
				if err != nil {
					if errors.Is(err, unix.EINVAL) {
						// The part was not a symlink, so assume that it's a
						// regular file. It is possible for it to be a
						// directory (if an attacker is swapping a directory
						// and non-directory at this subpath) but erroring out
						// here is better anyway.
						err = fmt.Errorf("%w: path component %q is invalid: %w", errPossibleAttack, part, unix.ENOTDIR)
					}
					return nil, "", err
				}

				linksWalked++
				if linksWalked > maxSymlinkLimit {
					return nil, "", &os.PathError{Op: "partialLookupInRoot", Path: logicalRootPath + "/" + unsafePath, Err: unix.ELOOP}
				}

				// Swap out the symlink's component for the link entry itself.
				if err := symlinkStack.SwapLink(part, currentDir, oldRemainingPath, linkDest); err != nil {
					return nil, "", fmt.Errorf("walking into symlink %q failed: push symlink: %w", part, err)
				}

				// Update our logical remaining path.
				remainingPath = linkDest + "/" + remainingPath
				// Absolute symlinks reset any work we've already done.
				if path.IsAbs(linkDest) {
					// Jump to root.
					rootClone, err := dupFile(root)
					if err != nil {
						return nil, "", fmt.Errorf("clone root fd: %w", err)
					}
					_ = currentDir.Close()
					currentDir = rootClone
					currentPath = "/"
				}
			default:
				// For any other file type, we can't walk further and so we've
				// hit the end of the lookup. The handling is very similar to
				// ENOENT from openat(2), except that we return a handle to the
				// component we just walked into (and we drop the component
				// from the symlink stack).
				_ = currentDir.Close()

				// The part existed, so drop it from the symlink stack.
				if err := symlinkStack.PopPart(part); err != nil {
					return nil, "", fmt.Errorf("walking into non-directory %q failed: %w", part, err)
				}

				// If there are any remaining components in the symlink stack,
				// we are still within a symlink resolution and thus we hit a
				// dangling symlink. So pretend that the first symlink in the
				// stack we hit was an ENOENT (to match openat2).
				if oldDir, remainingPath, ok := symlinkStack.PopTopSymlink(); ok {
					_ = nextDir.Close()
					return oldDir, remainingPath, nil
				}

				// The current component exists, so return it.
				return nextDir, remainingPath, nil
			}

		case errors.Is(err, os.ErrNotExist):
			// If there are any remaining components in the symlink stack, we
			// are still within a symlink resolution and thus we hit a dangling
			// symlink. So pretend that the first symlink in the stack we hit
			// was an ENOENT (to match openat2).
			if oldDir, remainingPath, ok := symlinkStack.PopTopSymlink(); ok {
				_ = currentDir.Close()
				return oldDir, remainingPath, nil
			}
			// We have hit a final component that doesn't exist, so we have our
			// partial open result. Note that we have to use the OLD remaining
			// path, since the lookup failed.
			return currentDir, oldRemainingPath, nil

		default:
			return nil, "", err
		}
	}
	// All of the components existed!
	return currentDir, "", nil
}

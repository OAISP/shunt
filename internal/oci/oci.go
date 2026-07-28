// Package oci reads the content-addressed blob store inside an OCI layout.
//
// The layout is the format shunt builds, ships, verifies and packages, so four
// places had grown their own copy of "list blobs/sha256" and "check a blob
// against its own filename". They are the same two operations, and the second
// one is the integrity check the whole registry-free design rests on: a blob's
// filename *is* its content hash, so re-deriving the hash proves the transfer.
package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// BlobDir is where a layout keeps its content-addressed blobs.
func BlobDir(layout string) string { return filepath.Join(layout, "blobs", "sha256") }

// Blobs lists a layout's blobs as digest → size.
func Blobs(layout string) (map[string]int64, error) {
	ents, err := os.ReadDir(BlobDir(layout))
	if err != nil {
		return nil, fmt.Errorf("read layout %s: %w", BlobDir(layout), err)
	}
	out := make(map[string]int64, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		out[e.Name()] = fi.Size()
	}
	return out, nil
}

// VerifyBlob rehashes a blob and checks it against its own filename, returning
// its size.
//
// rsync decides what to skip from size and mtime alone, which a same-size
// corruption slips past — this is what catches it.
func VerifyBlob(path, digest string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		return 0, fmt.Errorf("blob %s is corrupt (hashes to %s)", Short(digest), Short(got))
	}
	return n, nil
}

// Short truncates a digest to something readable in an error message.
func Short(digest string) string {
	if len(digest) > 16 {
		return digest[:16]
	}
	return digest
}

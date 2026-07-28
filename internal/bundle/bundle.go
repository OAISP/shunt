// Package bundle packages a built release into one file that can be carried to
// a host by some means other than a live connection to the machine that built
// it — a USB stick, an approval queue, an air-gapped network.
//
// It is deliberately the smallest thing that works. A bundle is an uncompressed
// tar holding a description, the OCI layouts, and any artifacts. It carries no
// helper binary, because the CLI applying it already embeds one; it carries no
// checksum of its own, because every blob inside is content-addressed and
// rehashed on load; and it is always complete rather than a delta against some
// particular host, because a delta is only valid against the state it was
// computed from and that is a promise a file sitting in a queue cannot keep.
package bundle

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/OAISP/shunt/internal/manifest"
	"github.com/OAISP/shunt/internal/release"
)

// Version is the bundle format. A reader refuses anything it does not know,
// which turns a format change into a clear error rather than a misread archive.
const Version = 1

// metaName is the first entry in the archive, so a reader can learn what it is
// holding without buffering the whole thing.
const metaName = "bundle.json"

// Meta describes a bundle. It is everything needed to apply the release except
// the secret values, which are resolved on the machine that applies it.
type Meta struct {
	Version int    `json:"version"`
	Created string `json:"created"`

	// Host is where this release was built to go. `shunt apply` uses it unless
	// told otherwise, so applying needs no shunt.toml.
	Host string `json:"host"`

	// Secrets is the provider block, never the values. A bundle that carried
	// production credentials would be the worst possible thing to leave on a
	// USB stick, and encrypting them into it only moves the key problem.
	Secrets *manifest.Secrets `json:"secrets,omitempty"`

	// Spec is the release, with secret values stripped. Its Images point at
	// layouts inside the archive.
	Spec *release.Spec `json:"spec"`
}

// Contents names what a bundle should hold, resolved on the building machine.
type Contents struct {
	Meta Meta

	// ImageDirs maps a manifest image name to its local OCI layout directory.
	ImageDirs map[string]string

	// ArtifactPaths maps an artifact name to its local file or directory.
	ArtifactPaths map[string]string
}

// Write serialises a bundle to w.
func Write(w io.Writer, c Contents) error {
	c.Meta.Version = Version
	tw := tar.NewWriter(w)

	b, err := json.MarshalIndent(c.Meta, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(tw, metaName, b, 0o644); err != nil {
		return err
	}

	for _, name := range sortedKeys(c.ImageDirs) {
		if err := writeTree(tw, path.Join("images", name), c.ImageDirs[name]); err != nil {
			return fmt.Errorf("image %s: %w", name, err)
		}
	}
	for _, name := range sortedKeys(c.ArtifactPaths) {
		if err := writeTree(tw, path.Join("artifacts", name), c.ArtifactPaths[name]); err != nil {
			return fmt.Errorf("artifact %s: %w", name, err)
		}
	}
	return tw.Close()
}

// Extracted is an opened bundle: its description, plus where its payload was
// unpacked on disk.
type Extracted struct {
	Meta          Meta
	ImageDirs     map[string]string
	ArtifactPaths map[string]string
}

// Read unpacks a bundle into dir and returns what it contained.
func Read(r io.Reader, dir string) (*Extracted, error) {
	out := &Extracted{ImageDirs: map[string]string{}, ArtifactPaths: map[string]string{}}
	tr := tar.NewReader(r)
	seen := map[string]bool{}

	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read bundle: %w", err)
		}
		// A tar entry naming ../ would write outside the extraction directory.
		clean, err := safeJoin(dir, h.Name)
		if err != nil {
			return nil, err
		}

		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(clean, 0o755); err != nil {
				return nil, err
			}
			continue
		case tar.TypeReg:
		default:
			// Layouts hold plain files, and anything else in an artifact is not
			// something to reconstruct silently.
			return nil, fmt.Errorf("bundle contains an unsupported entry %q", h.Name)
		}

		if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(h.Mode).Perm())
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return nil, err
		}
		f.Close()

		if h.Name == metaName {
			b, err := os.ReadFile(clean)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(b, &out.Meta); err != nil {
				return nil, fmt.Errorf("bundle.json: %w", err)
			}
			if out.Meta.Version != Version {
				return nil, fmt.Errorf("this bundle is format v%d and this shunt speaks v%d — use a matching version",
					out.Meta.Version, Version)
			}
			continue
		}
		// Record the top-level name each payload path belongs to.
		if top, name, ok := splitPayload(h.Name); ok && !seen[top+"/"+name] {
			seen[top+"/"+name] = true
			switch top {
			case "images":
				out.ImageDirs[name] = filepath.Join(dir, "images", name)
			case "artifacts":
				out.ArtifactPaths[name] = filepath.Join(dir, "artifacts", name)
			}
		}
	}

	if out.Meta.Spec == nil {
		return nil, fmt.Errorf("bundle is missing %s", metaName)
	}
	return out, nil
}

// splitPayload picks the section and entry name out of "images/app/index.json".
func splitPayload(name string) (top, entry string, ok bool) {
	parts := strings.SplitN(path.Clean(name), "/", 3)
	if len(parts) < 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// safeJoin refuses a tar entry that would escape the extraction directory.
func safeJoin(dir, name string) (string, error) {
	clean := filepath.Join(dir, filepath.FromSlash(path.Clean("/"+name)))
	if !strings.HasPrefix(clean, filepath.Clean(dir)+string(os.PathSeparator)) {
		return "", fmt.Errorf("bundle entry %q would write outside the bundle directory", name)
	}
	return clean, nil
}

func writeFile(tw *tar.Writer, name string, b []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: mode, Size: int64(len(b)), Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

// writeTree adds a file, or every regular file under a directory, at prefix.
func writeTree(tw *tar.Writer, prefix, src string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return copyInto(tw, prefix, src, fi)
	}
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file; a bundle holds files and directories", p)
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		return copyInto(tw, path.Join(prefix, filepath.ToSlash(rel)), p, info)
	})
}

func copyInto(tw *tar.Writer, name, src string, fi os.FileInfo) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: int64(fi.Mode().Perm()), Size: fi.Size(), Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Sorted so the same release always produces a byte-identical archive.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

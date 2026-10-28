package main

import (
	"archive/tar"
	"encoding/json"
	"io"
	"os"
	"testing"
)

// wave3BundlePayloads reads every entry of a backup bundle tar into a
// map keyed by entry name. It is a test-only reader for bundles built
// by createBackup or writeTestBundle.
func wave3BundlePayloads(t *testing.T, bundlePath string) map[string][]byte {
	t.Helper()
	fh, err := os.Open(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	out := map[string][]byte{}
	tr := tar.NewReader(fh)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = data
	}
	return out
}

// wave3BundleManifest parses the manifest.json entry of a bundle.
func wave3BundleManifest(t *testing.T, bundlePath string) backupManifest {
	t.Helper()
	payloads := wave3BundlePayloads(t, bundlePath)
	raw, ok := payloads[backupEntryManifest]
	if !ok {
		t.Fatal("bundle has no manifest.json entry")
	}
	var manifest backupManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("bundle manifest does not parse: %v", err)
	}
	return manifest
}

// wave3ManifestEntrySize returns the recorded size of one manifest
// entry, or fails the test when the entry is absent.
func wave3ManifestEntrySize(t *testing.T, manifest backupManifest, name string) int64 {
	t.Helper()
	for _, f := range manifest.Files {
		if f.Name == name {
			return f.Size
		}
	}
	t.Fatalf("manifest has no entry %q (entries=%v)", name, manifest.Files)
	return 0
}

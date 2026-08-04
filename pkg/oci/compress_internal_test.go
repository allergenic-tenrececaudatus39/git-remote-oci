package oci

import (
	"testing"
)

func TestPackfileCompression(t *testing.T) {
	rawPayload := []byte("hello-git-packfile-data-compression-test-payload-1234567890")

	modes := []string{"gzip", "zstd", "none"}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			compressed, mediaType, err := compressPackfile(rawPayload, mode)
			if err != nil {
				t.Fatalf("compressPackfile failed for mode %s: %v", mode, err)
			}

			decompressed, err := decompressPackfile(compressed, mediaType)
			if err != nil {
				t.Fatalf("decompressPackfile failed for mode %s: %v", mode, err)
			}

			if string(decompressed) != string(rawPayload) {
				t.Errorf("Mismatch in decompressed payload: expected %q, got %q", rawPayload, decompressed)
			}
		})
	}
}

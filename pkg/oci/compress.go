package oci

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const (
	MediaTypeGitPackfile     = "application/vnd.git.repository.packfile.v1"
	MediaTypeGitPackfileGzip = "application/vnd.git.repository.packfile.v1+gzip"
	MediaTypeGitPackfileZstd = "application/vnd.git.repository.packfile.v1+zstd"
)

var (
	bytesBufferPool = sync.Pool{
		New: func() any {
			return new(bytes.Buffer)
		},
	}

	gzipWriterPool = sync.Pool{
		New: func() any {
			return gzip.NewWriter(io.Discard)
		},
	}

	zstdEncoderPool = sync.Pool{
		New: func() any {
			zw, _ := zstd.NewWriter(io.Discard,
				zstd.WithEncoderConcurrency(runtime.NumCPU()),
				zstd.WithEncoderLevel(zstd.SpeedFastest),
			)
			return zw
		},
	}
)

func getBytesBuffer() *bytes.Buffer {
	buf, ok := bytesBufferPool.Get().(*bytes.Buffer)
	if !ok {
		buf = new(bytes.Buffer)
	}
	buf.Reset()
	return buf
}

// getGzipWriter returns a pooled gzip.Writer already reset to w.
func getGzipWriter(w io.Writer) *gzip.Writer {
	gw, ok := gzipWriterPool.Get().(*gzip.Writer)
	if !ok {
		gw = gzip.NewWriter(w)
		return gw
	}
	gw.Reset(w)
	return gw
}

// getZstdEncoder returns a pooled zstd.Encoder already reset to w.
func getZstdEncoder(w io.Writer) *zstd.Encoder {
	zw, ok := zstdEncoderPool.Get().(*zstd.Encoder)
	if !ok {
		zw, _ = zstd.NewWriter(w,
			zstd.WithEncoderConcurrency(runtime.NumCPU()),
			zstd.WithEncoderLevel(zstd.SpeedFastest),
		)
		return zw
	}
	zw.Reset(w)
	return zw
}

func putBytesBuffer(buf *bytes.Buffer) {
	if buf != nil {
		buf.Reset()
		bytesBufferPool.Put(buf)
	}
}

type pooledGzipWriter struct {
	gw *gzip.Writer
}

func (p *pooledGzipWriter) Write(b []byte) (int, error) {
	return p.gw.Write(b)
}

func (p *pooledGzipWriter) Close() error {
	err := p.gw.Close()
	gzipWriterPool.Put(p.gw)
	return err
}

type pooledZstdWriter struct {
	zw *zstd.Encoder
}

func (p *pooledZstdWriter) Write(b []byte) (int, error) {
	return p.zw.Write(b)
}

func (p *pooledZstdWriter) Close() error {
	err := p.zw.Close()
	zstdEncoderPool.Put(p.zw)
	return err
}

// compressPackfile compresses raw packfile data using the specified mode ("gzip", "zstd", or "none").
// Returns compressed bytes and the corresponding OCI media type. Uses sync.Pool for buffer reuse.
func compressPackfile(data []byte, mode string) ([]byte, string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "gzip":
		buf := getBytesBuffer()
		defer putBytesBuffer(buf)

		gw := getGzipWriter(buf)
		if _, err := gw.Write(data); err != nil {
			gzipWriterPool.Put(gw)
			return nil, "", fmt.Errorf("gzip compression failed: %w", err)
		}
		if err := gw.Close(); err != nil {
			gzipWriterPool.Put(gw)
			return nil, "", fmt.Errorf("gzip writer close failed: %w", err)
		}
		res := append([]byte(nil), buf.Bytes()...)
		gzipWriterPool.Put(gw)
		return res, MediaTypeGitPackfileGzip, nil

	case "zstd":
		buf := getBytesBuffer()
		defer putBytesBuffer(buf)

		zw := getZstdEncoder(buf)
		if _, err := zw.Write(data); err != nil {
			_ = zw.Close()
			zstdEncoderPool.Put(zw)
			return nil, "", fmt.Errorf("zstd compression failed: %w", err)
		}
		if err := zw.Close(); err != nil {
			zstdEncoderPool.Put(zw)
			return nil, "", fmt.Errorf("zstd writer close failed: %w", err)
		}
		res := append([]byte(nil), buf.Bytes()...)
		zstdEncoderPool.Put(zw)
		return res, MediaTypeGitPackfileZstd, nil

	case "none", "raw", "":
		return data, MediaTypeGitPackfile, nil

	default:
		return nil, "", fmt.Errorf("unsupported compression mode: %s", mode)
	}
}

type nopWriteCloser struct {
	io.Writer
}

func (n *nopWriteCloser) Close() error {
	return nil
}

// compressedMediaType reports the layer media type a compression mode produces,
// without creating a writer. Callers that must know the media type before they
// begin streaming need it separately from CompressStream.
func compressedMediaType(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "gzip":
		return MediaTypeGitPackfileGzip, nil
	case "zstd":
		return MediaTypeGitPackfileZstd, nil
	case "none", "raw", "":
		return MediaTypeGitPackfile, nil
	default:
		return "", fmt.Errorf("unsupported compression mode: %s", mode)
	}
}

// CompressStream wraps an io.Writer with a pooled compression writer for the specified mode.
func CompressStream(w io.Writer, mode string) (io.WriteCloser, string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "gzip":
		return &pooledGzipWriter{gw: getGzipWriter(w)}, MediaTypeGitPackfileGzip, nil

	case "zstd":
		return &pooledZstdWriter{zw: getZstdEncoder(w)}, MediaTypeGitPackfileZstd, nil

	case "none", "raw", "":
		return &nopWriteCloser{Writer: w}, MediaTypeGitPackfile, nil

	default:
		return nil, "", fmt.Errorf("unsupported compression mode: %s", mode)
	}
}

// maxDecompressedSize bounds how much a single compressed registry layer may
// expand to. Registry content is untrusted, and a small layer can otherwise
// decompress to an arbitrary amount of memory.
const maxDecompressedSize = 8 << 30 // 8 GiB

// errTooLarge reports that a stream exceeded maxDecompressedSize.
var errTooLarge = fmt.Errorf("decompressed layer exceeds the %d byte limit", int64(maxDecompressedSize))

// readAllLimited reads r to EOF, refusing to buffer more than limit bytes.
func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errTooLarge
	}
	return data, nil
}

// decompressPackfile decompresses layer bytes based on media type or magic header bytes.
func decompressPackfile(data []byte, mediaType string) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}

	cleanType := strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0]))

	// 1. Check Media Type or Gzip Magic Header (0x1f 0x8b)
	if cleanType == MediaTypeGitPackfileGzip ||
		(len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b) {
		gr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer func() { _ = gr.Close() }()
		return readAllLimited(gr, maxDecompressedSize)
	}

	// 2. Check Media Type or Zstd Magic Header (0x28 0xb5 0x2f 0xfd)
	if cleanType == MediaTypeGitPackfileZstd ||
		(len(data) >= 4 && data[0] == 0x28 && data[1] == 0xb5 && data[2] == 0x2f && data[3] == 0xfd) {
		zr, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("failed to create zstd reader: %w", err)
		}
		defer zr.Close()
		return readAllLimited(zr, maxDecompressedSize)
	}

	// 3. Raw / Uncompressed
	return data, nil
}

// decompReadCloser pairs a decompressing reader with the underlying stream.
//
// Both must be closed. zstd.NewReader starts worker goroutines that are only
// released by closing the decoder, so closing just the underlying stream leaks
// them once per fetched packfile; gzip's Close is what verifies the trailing
// CRC and length, so skipping it silently accepts a truncated or corrupt
// stream.
type decompReadCloser struct {
	r           io.Reader
	c           io.Closer
	closeDecomp func() error
}

func (d *decompReadCloser) Read(p []byte) (int, error) {
	return d.r.Read(p)
}

func (d *decompReadCloser) Close() error {
	var decompErr error
	if d.closeDecomp != nil {
		decompErr = d.closeDecomp()
	}
	closeErr := d.c.Close()
	if decompErr != nil {
		return decompErr
	}
	return closeErr
}

// DecompressStream wraps an io.ReadCloser with an automatic decompressor reader based on media type.
func DecompressStream(rc io.ReadCloser, mediaType string) (io.ReadCloser, error) {
	cleanType := strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0]))

	if cleanType == MediaTypeGitPackfileGzip {
		gr, err := gzip.NewReader(rc)
		if err != nil {
			_ = rc.Close()
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		return &decompReadCloser{r: gr, c: rc, closeDecomp: gr.Close}, nil
	}

	if cleanType == MediaTypeGitPackfileZstd {
		zr, err := zstd.NewReader(rc)
		if err != nil {
			_ = rc.Close()
			return nil, fmt.Errorf("failed to create zstd reader: %w", err)
		}
		// zstd.Decoder.Close has no error to report, but it is what releases
		// the decoder's worker goroutines.
		return &decompReadCloser{r: zr, c: rc, closeDecomp: func() error { zr.Close(); return nil }}, nil
	}

	return rc, nil
}

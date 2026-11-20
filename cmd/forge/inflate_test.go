package main

import (
	"bytes"
	"compress/zlib"
	"math/rand"
	"strings"
	"testing"
)

func compressZlibLevel(t *testing.T, data []byte, level int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zlib.NewWriterLevel(&buf, level)
	if err != nil {
		t.Fatalf("NewWriterLevel(%d): %v", level, err)
	}
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func TestFastInflateZlibParity(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	randBytes := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.Intn(256))
		}
		return b
	}
	mixedBytes := func(n int) []byte {
		b := make([]byte, n)
		for i := 0; i < n; i++ {
			if (i/128)%2 == 0 {
				b[i] = byte('a' + (i % 26))
			} else {
				b[i] = byte(rng.Intn(256))
			}
		}
		return b
	}
	skewedBytes := func(n int) []byte {
		// Strongly skewed frequencies trigger code lengths > 9 bits.
		b := make([]byte, 0, n)
		for k := 0; len(b) < n; k++ {
			b = append(b, bytes.Repeat([]byte{'A'}, 100)...)
			for sym := 0; sym < 250 && len(b) < n; sym++ {
				if k%(sym+1) == 0 {
					b = append(b, byte(sym))
				}
			}
		}
		return b[:n]
	}

	cases := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"one_byte", []byte("x")},
		{"rle_overlap", bytes.Repeat([]byte("abc"), 500)},
		{"1kb_text", []byte(strings.Repeat("bench payload line 0123456789abcdef\n", 30))},
		{"incompressible_1kb", randBytes(1024)},
		{"mixed_20kb", mixedBytes(20000)},
		{"skewed_32kb", skewedBytes(32768)},
		{"boundary_64kb", mixedBytes(65536)},
		{"fallback_70kb", mixedBytes(70000)},
	}

	levels := []int{zlib.NoCompression, zlib.BestSpeed, zlib.DefaultCompression, zlib.BestCompression}

	for _, tc := range cases {
		for _, lvl := range levels {
			z := compressZlibLevel(t, tc.data, lvl)
			// Append trailing garbage to make sure consumed stops strictly after zlib trailer.
			streamWithTail := append(append([]byte(nil), z...), []byte("TRAILING_BYTES")...)

			got, consumed, err := fastInflateZlib(streamWithTail, len(tc.data))
			if err != nil {
				t.Fatalf("%s lvl=%d: fastInflateZlib failed: %v", tc.name, lvl, err)
			}
			if consumed != len(z) {
				t.Fatalf("%s lvl=%d: consumed=%d want=%d", tc.name, lvl, consumed, len(z))
			}
			if !bytes.Equal(got, tc.data) {
				t.Fatalf("%s lvl=%d: output mismatch (len %d vs %d)", tc.name, lvl, len(got), len(tc.data))
			}

			stdOut, stdConsumed, stdErr := inflateExactStdlib(streamWithTail, len(tc.data))
			if stdErr != nil {
				t.Fatalf("%s lvl=%d: stdlib failed: %v", tc.name, lvl, stdErr)
			}
			if stdConsumed != consumed || !bytes.Equal(stdOut, got) {
				t.Fatalf("%s lvl=%d: parity mismatch vs stdlib", tc.name, lvl)
			}
		}
	}
}

func TestFastInflateZlibConsecutive(t *testing.T) {
	a := []byte(strings.Repeat("blob content 1 ", 80))
	b := []byte(strings.Repeat("tree entry 2 ", 12))
	c := []byte("commit abcdef0123456789\n")

	za := compressZlibLevel(t, a, zlib.BestSpeed)
	zb := compressZlibLevel(t, b, zlib.DefaultCompression)
	zc := compressZlibLevel(t, c, zlib.BestCompression)

	joined := append(append(append([]byte(nil), za...), zb...), zc...)
	off := 0
	for i, exp := range [][]byte{a, b, c} {
		out, n, err := fastInflateZlib(joined[off:], len(exp))
		if err != nil {
			t.Fatalf("stream %d err: %v", i, err)
		}
		if !bytes.Equal(out, exp) {
			t.Fatalf("stream %d mismatch", i)
		}
		off += n
	}
	if off != len(joined) {
		t.Fatalf("total consumed %d != %d", off, len(joined))
	}
}

func TestFastInflateZlibErrors(t *testing.T) {
	payload := []byte(strings.Repeat("sample pack object body ", 60))
	z := compressZlibLevel(t, payload, zlib.DefaultCompression)

	if _, _, err := fastInflateZlib(z[:len(z)-1], len(payload)); err == nil {
		t.Fatalf("expected error for truncated adler32")
	}
	if _, _, err := fastInflateZlib(z[:len(z)/2], len(payload)); err == nil {
		t.Fatalf("expected error for truncated body")
	}
	if _, _, err := fastInflateZlib(z[:4], len(payload)); err == nil {
		t.Fatalf("expected error for short stream")
	}
	if _, _, err := fastInflateZlib(z, len(payload)+1); err == nil {
		t.Fatalf("expected error when expectedSize is too large")
	}
	if _, _, err := fastInflateZlib(z, len(payload)-1); err == nil {
		t.Fatalf("expected error when expectedSize is too small")
	}

	badAdler := append([]byte(nil), z...)
	badAdler[len(badAdler)-1] ^= 0xff
	if _, _, err := fastInflateZlib(badAdler, len(payload)); err == nil {
		t.Fatalf("expected error for bad adler32 checksum")
	}

	badFcheck := append([]byte(nil), z...)
	badFcheck[1] ^= 0x01
	if _, _, err := fastInflateZlib(badFcheck, len(payload)); err == nil {
		t.Fatalf("expected error for bad zlib header fcheck")
	}

	// Stored block invalid NLEN
	badStored := []byte{0x78, 0x01, 0x01, 0x03, 0x00, 0x00, 0x00, 'a', 'b', 'c', 0, 0, 0, 0}
	if _, _, err := fastInflateZlib(badStored, 3); err == nil {
		t.Fatalf("expected error for invalid stored block nlen")
	}
}

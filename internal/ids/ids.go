// Package ids generates opaque, time-sortable identifiers with a type prefix
// (ULID layout: 48-bit millisecond time + 80 random bits, Crockford base32).
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"strings"
	"time"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New returns prefix + "_" + 26-char ULID, e.g. "bld_01JB6...".
func New(prefix string) string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	binary.BigEndian.PutUint16(b[0:2], uint16(ms>>32))
	binary.BigEndian.PutUint32(b[2:6], uint32(ms))
	if _, err := rand.Read(b[6:]); err != nil {
		panic(err)
	}
	return prefix + "_" + encode(b)
}

func encode(b [16]byte) string {
	// 128 bits -> 26 base32 chars (first char carries 3 bits).
	var sb strings.Builder
	hi := binary.BigEndian.Uint64(b[0:8])
	lo := binary.BigEndian.Uint64(b[8:16])
	out := make([]byte, 26)
	for i := 25; i >= 0; i-- {
		out[i] = alphabet[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	sb.Write(out)
	return sb.String()
}

// Secret returns n random bytes encoded in Crockford base32 (for tokens).
func Secret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	var sb strings.Builder
	var acc uint32
	bits := 0
	for _, x := range b {
		acc = acc<<8 | uint32(x)
		bits += 8
		for bits >= 5 {
			sb.WriteByte(alphabet[(acc>>(bits-5))&31])
			bits -= 5
		}
	}
	if bits > 0 {
		sb.WriteByte(alphabet[(acc<<(5-bits))&31])
	}
	return sb.String()
}

// Valid reports whether s looks like an id with the given prefix.
func Valid(s, prefix string) bool {
	if !strings.HasPrefix(s, prefix+"_") || len(s) != len(prefix)+27 {
		return false
	}
	for _, c := range s[len(prefix)+1:] {
		if !strings.ContainsRune(alphabet, c) {
			return false
		}
	}
	return true
}

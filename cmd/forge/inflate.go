package main

import (
	"hash/adler32"
)

const (
	huffRootBits = 9
	huffRootSize = 1 << huffRootBits
	huffRootMask = huffRootSize - 1
	huffMaxBits  = 15
	huffSubFlag  = uint32(1) << 31
)

type huffTable struct {
	root [huffRootSize]uint32
	sub  [1024]uint32
	nSub uint32
}

var (
	lenBase = [29]int{
		3, 4, 5, 6, 7, 8, 9, 10, 11, 13,
		15, 17, 19, 23, 27, 31, 35, 43, 51, 59,
		67, 83, 99, 115, 131, 163, 195, 227, 258,
	}
	lenExtra = [29]uint{
		0, 0, 0, 0, 0, 0, 0, 0, 1, 1,
		1, 1, 2, 2, 2, 2, 3, 3, 3, 3,
		4, 4, 4, 4, 5, 5, 5, 5, 0,
	}
	distBase = [30]int{
		1, 2, 3, 4, 5, 7, 9, 13, 17, 25,
		33, 49, 65, 97, 129, 193, 257, 385, 513, 769,
		1025, 1537, 2049, 3073, 4097, 6145, 8193, 12289, 16385, 24577,
	}
	distExtra = [30]uint{
		0, 0, 0, 0, 1, 1, 2, 2, 3, 3,
		4, 4, 5, 5, 6, 6, 7, 7, 8, 8,
		9, 9, 10, 10, 11, 11, 12, 12, 13, 13,
	}
	clOrder = [19]int{
		16, 17, 18, 0, 8, 7, 9, 6, 10, 5,
		11, 4, 12, 3, 13, 2, 14, 1, 15,
	}

	fixedLitTable  huffTable
	fixedDistTable huffTable
	defaultScratch zlibScratch
)

func init() {
	var litLens [288]uint8
	for i := 0; i <= 143; i++ {
		litLens[i] = 8
	}
	for i := 144; i <= 255; i++ {
		litLens[i] = 9
	}
	for i := 256; i <= 279; i++ {
		litLens[i] = 7
	}
	for i := 280; i <= 287; i++ {
		litLens[i] = 8
	}
	buildHuffTable(&fixedLitTable, litLens[:])

	var distLens [32]uint8
	for i := 0; i < 32; i++ {
		distLens[i] = 5
	}
	buildHuffTable(&fixedDistTable, distLens[:])
}

type zlibScratch struct {
	clLens   [19]uint8
	litDist  [320]uint8
	clTable  huffTable
	litTable huffTable
	dstTable huffTable
}

func reverseBits(code uint32, n uint) uint32 {
	var r uint32
	for i := uint(0); i < n; i++ {
		r = (r << 1) | ((code >> i) & 1)
	}
	return r
}

func buildHuffTable(t *huffTable, lengths []uint8) bool {
	var blCount [huffMaxBits + 1]int
	maxLen := 0
	for _, l := range lengths {
		if l > 0 {
			if int(l) > huffMaxBits {
				return false
			}
			blCount[l]++
			if int(l) > maxLen {
				maxLen = int(l)
			}
		}
	}

	clear(t.root[:])
	t.nSub = 0
	if maxLen == 0 {
		return true
	}

	left := 1
	for bits := 1; bits <= huffMaxBits; bits++ {
		left <<= 1
		left -= blCount[bits]
		if left < 0 {
			return false
		}
	}

	var nextCode [huffMaxBits + 1]uint32
	var code uint32
	for bits := 1; bits <= huffMaxBits; bits++ {
		code = (code + uint32(blCount[bits-1])) << 1
		nextCode[bits] = code
	}

	var revCodes [320]uint32
	var rootMaxLen [huffRootSize]uint8
	nSyms := len(lengths)

	for sym := 0; sym < nSyms; sym++ {
		l := int(lengths[sym])
		if l == 0 {
			continue
		}
		c := nextCode[l]
		nextCode[l]++
		rev := reverseBits(c, uint(l))
		revCodes[sym] = rev
		if l <= huffRootBits {
			val := (uint32(sym) << 8) | uint32(l)
			step := uint32(1) << uint(l)
			for idx := rev; idx < huffRootSize; idx += step {
				t.root[idx] = val
			}
		} else {
			r := rev & huffRootMask
			if uint8(l) > rootMaxLen[r] {
				rootMaxLen[r] = uint8(l)
			}
		}
	}

	if maxLen <= huffRootBits {
		return true
	}

	for r := 0; r < huffRootSize; r++ {
		ml := int(rootMaxLen[r])
		if ml == 0 {
			continue
		}
		subBits := ml - huffRootBits
		subSize := uint32(1) << uint(subBits)
		if int(t.nSub+subSize) > len(t.sub) {
			return false
		}
		subOff := t.nSub
		clear(t.sub[subOff : subOff+subSize])
		t.nSub += subSize
		t.root[r] = huffSubFlag | (subOff << 8) | uint32(subBits)
	}

	for sym := 0; sym < nSyms; sym++ {
		l := int(lengths[sym])
		if l <= huffRootBits {
			continue
		}
		rev := revCodes[sym]
		re := t.root[rev&huffRootMask]
		subBits := re & 0xff
		subOff := (re >> 8) & 0xffff
		subSize := uint32(1) << subBits
		val := (uint32(sym) << 8) | uint32(l)
		step := uint32(1) << uint(l-huffRootBits)
		for idx := rev >> huffRootBits; idx < subSize; idx += step {
			t.sub[subOff+idx] = val
		}
	}

	return true
}

func decodeHuffSym(t *huffTable, src []byte, pos *int, bits *uint64, nbits *uint) (int, bool) {
	for *nbits < huffMaxBits && *pos < len(src) {
		*bits |= uint64(src[*pos]) << *nbits
		*pos++
		*nbits += 8
	}
	e := t.root[*bits&huffRootMask]
	if (e & huffSubFlag) != 0 {
		subBits := e & 0xff
		subOff := (e >> 8) & 0xffff
		e = t.sub[subOff+uint32((*bits>>huffRootBits)&((uint64(1)<<subBits)-1))]
	}
	clen := uint(e & 0xff)
	if clen == 0 || clen > *nbits {
		return 0, false
	}
	*bits >>= clen
	*nbits -= clen
	return int(e >> 8), true
}

func fastInflateZlib(src []byte, expectedSize int) ([]byte, int, error) {
	return fastInflateZlibWithScratch(&defaultScratch, src, expectedSize)
}

func fastInflateZlibWithScratch(sc *zlibScratch, src []byte, expectedSize int) ([]byte, int, error) {
	if expectedSize > 65536 {
		return inflateExactStdlib(src, expectedSize)
	}
	if expectedSize < 0 || len(src) < 6 {
		return nil, 0, ErrPackFormat
	}
	cmf := src[0]
	flg := src[1]
	if (cmf&0x0f) != 8 || (cmf>>4) > 7 {
		return nil, 0, ErrPackFormat
	}
	if (uint16(cmf)*256+uint16(flg))%31 != 0 {
		return nil, 0, ErrPackFormat
	}
	if (flg & 0x20) != 0 {
		return inflateExactStdlib(src, expectedSize)
	}

	out := make([]byte, expectedSize)
	dstPos := 0
	pos := 2
	var bits uint64
	var nbits uint

	needBits := func(n uint) bool {
		for nbits < n && pos < len(src) {
			bits |= uint64(src[pos]) << nbits
			pos++
			nbits += 8
		}
		return nbits >= n
	}

	for {
		if !needBits(3) {
			return nil, 0, ErrPackFormat
		}
		bfinal := bits & 1
		btype := (bits >> 1) & 3
		bits >>= 3
		nbits -= 3

		switch btype {
		case 0:
			drop := nbits & 7
			bits >>= drop
			nbits -= drop
			pos -= int(nbits / 8)
			bits = 0
			nbits = 0
			if pos+4 > len(src) {
				return nil, 0, ErrPackFormat
			}
			l := uint16(src[pos]) | (uint16(src[pos+1]) << 8)
			nl := uint16(src[pos+2]) | (uint16(src[pos+3]) << 8)
			pos += 4
			if l != ^nl {
				return nil, 0, ErrPackFormat
			}
			ilen := int(l)
			if pos+ilen > len(src) || dstPos+ilen > expectedSize {
				return nil, 0, ErrPackFormat
			}
			copy(out[dstPos:dstPos+ilen], src[pos:pos+ilen])
			pos += ilen
			dstPos += ilen

		case 1, 2:
			litT := &fixedLitTable
			distT := &fixedDistTable
			if btype == 2 {
				if !needBits(14) {
					return nil, 0, ErrPackFormat
				}
				hlit := int(bits&0x1f) + 257
				hdist := int((bits>>5)&0x1f) + 1
				hclen := int((bits>>10)&0x0f) + 4
				bits >>= 14
				nbits -= 14
				if hlit > 286 || hdist > 30 || hclen > 19 {
					return nil, 0, ErrPackFormat
				}
				clear(sc.clLens[:])
				for i := 0; i < hclen; i++ {
					if !needBits(3) {
						return nil, 0, ErrPackFormat
					}
					sc.clLens[clOrder[i]] = uint8(bits & 7)
					bits >>= 3
					nbits -= 3
				}
				if !buildHuffTable(&sc.clTable, sc.clLens[:]) {
					return nil, 0, ErrPackFormat
				}

				totalCodes := hlit + hdist
				idx := 0
				for idx < totalCodes {
					sym, ok := decodeHuffSym(&sc.clTable, src, &pos, &bits, &nbits)
					if !ok {
						return nil, 0, ErrPackFormat
					}
					if sym < 16 {
						sc.litDist[idx] = uint8(sym)
						idx++
					} else {
						var repVal uint8
						var repCount int
						switch sym {
						case 16:
							if idx == 0 || !needBits(2) {
								return nil, 0, ErrPackFormat
							}
							repVal = sc.litDist[idx-1]
							repCount = 3 + int(bits&3)
							bits >>= 2
							nbits -= 2
						case 17:
							if !needBits(3) {
								return nil, 0, ErrPackFormat
							}
							repVal = 0
							repCount = 3 + int(bits&7)
							bits >>= 3
							nbits -= 3
						case 18:
							if !needBits(7) {
								return nil, 0, ErrPackFormat
							}
							repVal = 0
							repCount = 11 + int(bits&0x7f)
							bits >>= 7
							nbits -= 7
						default:
							return nil, 0, ErrPackFormat
						}
						if idx+repCount > totalCodes {
							return nil, 0, ErrPackFormat
						}
						for k := 0; k < repCount; k++ {
							sc.litDist[idx] = repVal
							idx++
						}
					}
				}

				if sc.litDist[256] == 0 {
					return nil, 0, ErrPackFormat
				}
				if !buildHuffTable(&sc.litTable, sc.litDist[:hlit]) {
					return nil, 0, ErrPackFormat
				}
				if !buildHuffTable(&sc.dstTable, sc.litDist[hlit:hlit+hdist]) {
					return nil, 0, ErrPackFormat
				}
				litT = &sc.litTable
				distT = &sc.dstTable
			}

			for {
				sym, ok := decodeHuffSym(litT, src, &pos, &bits, &nbits)
				if !ok {
					return nil, 0, ErrPackFormat
				}
				if sym < 256 {
					if dstPos >= expectedSize {
						return nil, 0, ErrPackFormat
					}
					out[dstPos] = byte(sym)
					dstPos++
					continue
				}
				if sym == 256 {
					break
				}
				lIdx := sym - 257
				if lIdx >= len(lenBase) {
					return nil, 0, ErrPackFormat
				}
				length := lenBase[lIdx]
				exL := lenExtra[lIdx]
				if exL > 0 {
					if !needBits(exL) {
						return nil, 0, ErrPackFormat
					}
					length += int(bits & ((uint64(1) << exL) - 1))
					bits >>= exL
					nbits -= exL
				}

				dsym, dok := decodeHuffSym(distT, src, &pos, &bits, &nbits)
				if !dok || dsym >= len(distBase) {
					return nil, 0, ErrPackFormat
				}
				dist := distBase[dsym]
				exD := distExtra[dsym]
				if exD > 0 {
					if !needBits(exD) {
						return nil, 0, ErrPackFormat
					}
					dist += int(bits & ((uint64(1) << exD) - 1))
					bits >>= exD
					nbits -= exD
				}

				if dist <= 0 || dist > dstPos || dstPos+length > expectedSize {
					return nil, 0, ErrPackFormat
				}
				if dist >= length {
					copy(out[dstPos:dstPos+length], out[dstPos-dist:dstPos-dist+length])
				} else {
					for i := 0; i < length; i++ {
						out[dstPos+i] = out[dstPos-dist+i]
					}
				}
				dstPos += length
			}

		default:
			return nil, 0, ErrPackFormat
		}

		if bfinal != 0 {
			break
		}
	}

	if dstPos != expectedSize {
		return nil, 0, ErrPackFormat
	}

	drop := nbits & 7
	bits >>= drop
	nbits -= drop
	pos -= int(nbits / 8)
	if pos+4 > len(src) {
		return nil, 0, ErrPackFormat
	}
	wantAdler := (uint32(src[pos]) << 24) | (uint32(src[pos+1]) << 16) | (uint32(src[pos+2]) << 8) | uint32(src[pos+3])
	if adler32.Checksum(out) != wantAdler {
		return nil, 0, ErrPackFormat
	}
	return out, pos + 4, nil
}

//go:build !linux

package main

import (
	"io"
	"net"
	"sync"
)

func StartServer(store *ForgeStore, expectedAuth []byte) (int, func(), func(), error) {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return 0, nil, nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port

	var stopOnce sync.Once
	shutdown := func() {
		stopOnce.Do(func() {
			_ = ln.Close()
		})
	}

	var dispatchMu sync.Mutex

	handleConn := func(conn net.Conn) {
		defer conn.Close()
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
		}

		rbuf := make([]byte, 16384)
		roff := 0
		rlen := 0
		var chunkSc []byte
		sent100 := false
		needHint := 0

		for {
			if needHint > 0 {
				neededCap := roff + needHint
				if neededCap > cap(rbuf) {
					grown := make([]byte, neededCap*2)
					if rlen > roff {
						copy(grown, rbuf[roff:rlen])
					}
					rlen -= roff
					roff = 0
					rbuf = grown
				}
			} else if rlen == len(rbuf) {
				if roff > 0 {
					rem := rlen - roff
					copy(rbuf[:rem], rbuf[roff:rlen])
					roff = 0
					rlen = rem
				} else {
					grown := make([]byte, len(rbuf)*2)
					copy(grown, rbuf[:rlen])
					rbuf = grown
				}
			}

			nr, rErr := conn.Read(rbuf[rlen:])
			if nr > 0 {
				rlen += nr
			} else if rErr != nil {
				if rErr != io.EOF || rlen == roff {
					return
				}
			}

			for roff < rlen {
				req, consumed, hint, ns, st := parseHTTPRequestInPlace(rbuf[roff:rlen], chunkSc, sent100)
				chunkSc = ns
				if st == parseSend100 {
					roff += consumed
					if err := writeAll(conn, expect100Msg); err != nil {
						return
					}
					sent100 = true
					continue
				}
				if st == parseNeedMore {
					roff += consumed
					needHint = hint
					break
				}
				if st == parseBad {
					return
				}

				roff += consumed
				sent100 = false
				needHint = 0

				dispatchMu.Lock()
				resp := dispatchRequest(&req, store, expectedAuth)
				wErr := writeAll(conn, resp)
				dispatchMu.Unlock()
				if wErr != nil || req.CloseConn {
					return
				}
			}

			if roff == rlen {
				roff = 0
				rlen = 0
				if cap(rbuf) > 131072 {
					rbuf = make([]byte, 16384)
				}
			}
			if rErr != nil {
				return
			}
		}
	}

	run := func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleConn(conn)
		}
	}

	return port, run, shutdown, nil
}

//go:build linux

package main

import (
	"runtime"
	"sync"
	"syscall"
)

const (
	epollRDHUP       = 0x2000
	defaultConnRBuf  = 16384
	maxPinnedConnBuf = 131072
)

type epConn struct {
	fd      int
	rbuf    []byte
	roff    int
	rlen    int
	wbuf    []byte
	woff    int
	chunkSc []byte
	sent100 bool
	closing bool
}

func StartServer(store *ForgeStore, expectedAuth []byte) (int, func(), func(), error) {
	lfd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, nil, nil, err
	}
	_ = syscall.SetsockoptInt(lfd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	if err := syscall.Bind(lfd, &syscall.SockaddrInet4{Port: 0, Addr: [4]byte{0, 0, 0, 0}}); err != nil {
		_ = syscall.Close(lfd)
		return 0, nil, nil, err
	}
	sa, err := syscall.Getsockname(lfd)
	if err != nil {
		_ = syscall.Close(lfd)
		return 0, nil, nil, err
	}
	port := sa.(*syscall.SockaddrInet4).Port
	if err := syscall.Listen(lfd, 512); err != nil {
		_ = syscall.Close(lfd)
		return 0, nil, nil, err
	}

	var pipeFds [2]int
	if err := syscall.Pipe2(pipeFds[:], syscall.O_NONBLOCK|syscall.O_CLOEXEC); err != nil {
		_ = syscall.Close(lfd)
		return 0, nil, nil, err
	}
	stopReadFd := pipeFds[0]
	stopWriteFd := pipeFds[1]

	epfd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		_ = syscall.Close(lfd)
		_ = syscall.Close(stopReadFd)
		_ = syscall.Close(stopWriteFd)
		return 0, nil, nil, err
	}

	if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, lfd, &syscall.EpollEvent{
		Events: syscall.EPOLLIN,
		Fd:     int32(lfd),
	}); err != nil {
		_ = syscall.Close(epfd)
		_ = syscall.Close(lfd)
		_ = syscall.Close(stopReadFd)
		_ = syscall.Close(stopWriteFd)
		return 0, nil, nil, err
	}
	if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, stopReadFd, &syscall.EpollEvent{
		Events: syscall.EPOLLIN,
		Fd:     int32(stopReadFd),
	}); err != nil {
		_ = syscall.Close(epfd)
		_ = syscall.Close(lfd)
		_ = syscall.Close(stopReadFd)
		_ = syscall.Close(stopWriteFd)
		return 0, nil, nil, err
	}

	var stopOnce sync.Once
	shutdown := func() {
		stopOnce.Do(func() {
			var b [1]byte
			b[0] = 1
			_, _ = syscall.Write(stopWriteFd, b[:])
			_ = syscall.Close(stopWriteFd)
		})
	}

	run := func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		conns := make([]*epConn, 256)
		var freeList []*epConn

		allocConn := func(fd int) *epConn {
			var c *epConn
			if n := len(freeList); n > 0 {
				c = freeList[n-1]
				freeList = freeList[:n-1]
			} else {
				c = &epConn{
					rbuf: make([]byte, defaultConnRBuf),
				}
			}
			c.fd = fd
			c.roff = 0
			c.rlen = 0
			c.wbuf = c.wbuf[:0]
			c.woff = 0
			c.sent100 = false
			c.closing = false
			if fd >= len(conns) {
				newCap := fd + 128
				grown := make([]*epConn, newCap)
				copy(grown, conns)
				conns = grown
			}
			conns[fd] = c
			return c
		}

		closeConn := func(c *epConn) {
			if c == nil || c.fd < 0 {
				return
			}
			fd := c.fd
			_ = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_DEL, fd, nil)
			_ = syscall.Close(fd)
			if fd < len(conns) {
				conns[fd] = nil
			}
			c.fd = -1
			if cap(c.rbuf) > maxPinnedConnBuf {
				c.rbuf = make([]byte, defaultConnRBuf)
			}
			if cap(c.wbuf) > maxPinnedConnBuf {
				c.wbuf = nil
			}
			if cap(c.chunkSc) > maxPinnedConnBuf {
				c.chunkSc = nil
			}
			freeList = append(freeList, c)
		}

		setEpollOut := func(c *epConn, wantOut bool) {
			ev := uint32(syscall.EPOLLIN | epollRDHUP)
			if wantOut {
				ev |= syscall.EPOLLOUT
			}
			_ = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_MOD, c.fd, &syscall.EpollEvent{
				Events: ev,
				Fd:     int32(c.fd),
			})
		}

		writeResponse := func(c *epConn, data []byte) bool {
			if len(c.wbuf) > c.woff {
				c.wbuf = append(c.wbuf, data...)
				return true
			}
			for len(data) > 0 {
				n, wErr := syscall.Write(c.fd, data)
				if n > 0 {
					data = data[n:]
					continue
				}
				if wErr == syscall.EINTR {
					continue
				}
				if wErr == syscall.EAGAIN || wErr == syscall.EWOULDBLOCK {
					c.wbuf = append(c.wbuf[:0], data...)
					c.woff = 0
					setEpollOut(c, true)
					return true
				}
				return false
			}
			return true
		}

		flushWBuf := func(c *epConn) bool {
			for c.woff < len(c.wbuf) {
				n, wErr := syscall.Write(c.fd, c.wbuf[c.woff:])
				if n > 0 {
					c.woff += n
					continue
				}
				if wErr == syscall.EINTR {
					continue
				}
				if wErr == syscall.EAGAIN || wErr == syscall.EWOULDBLOCK {
					return true
				}
				return false
			}
			c.wbuf = c.wbuf[:0]
			c.woff = 0
			setEpollOut(c, false)
			if c.closing {
				return false
			}
			return true
		}

		events := make([]syscall.EpollEvent, 128)
		for {
			nev, err := syscall.EpollWait(epfd, events, -1)
			if err != nil {
				if err == syscall.EINTR {
					continue
				}
				break
			}
			for i := 0; i < nev; i++ {
				ev := events[i]
				fd := int(ev.Fd)

				if fd == stopReadFd {
					var tmp [64]byte
					_, _ = syscall.Read(stopReadFd, tmp[:])
					for _, c := range conns {
						if c != nil && c.fd >= 0 {
							_ = syscall.Close(c.fd)
						}
					}
					_ = syscall.Close(epfd)
					_ = syscall.Close(lfd)
					_ = syscall.Close(stopReadFd)
					return
				}

				if fd == lfd {
					for {
						cfd, _, aErr := syscall.Accept4(lfd, syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC)
						if aErr != nil {
							if aErr == syscall.EINTR {
								continue
							}
							break
						}
						_ = syscall.SetsockoptInt(cfd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
						c := allocConn(cfd)
						if eErr := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, cfd, &syscall.EpollEvent{
							Events: syscall.EPOLLIN | epollRDHUP,
							Fd:     int32(cfd),
						}); eErr != nil {
							closeConn(c)
						}
					}
					continue
				}

				if fd < 0 || fd >= len(conns) {
					continue
				}
				c := conns[fd]
				if c == nil {
					continue
				}

				if (ev.Events & syscall.EPOLLERR) != 0 {
					closeConn(c)
					continue
				}

				if (ev.Events & syscall.EPOLLOUT) != 0 {
					if !flushWBuf(c) {
						closeConn(c)
						continue
					}
				}

				if (ev.Events & (syscall.EPOLLIN | epollRDHUP | syscall.EPOLLHUP)) != 0 {
					sawRDHUP := (ev.Events & (syscall.EPOLLHUP | epollRDHUP)) != 0
					sawEOF := false
					connClosed := false
					needHint := 0

					for {
						if needHint > 0 {
							neededCap := c.roff + needHint
							if neededCap > cap(c.rbuf) {
								grown := make([]byte, neededCap*2)
								if c.rlen > c.roff {
									copy(grown, c.rbuf[c.roff:c.rlen])
								}
								c.rlen -= c.roff
								c.roff = 0
								c.rbuf = grown
							} else if c.roff > 0 && c.rlen-c.roff < needHint && len(c.rbuf)-c.rlen < (needHint-(c.rlen-c.roff)) {
								rem := c.rlen - c.roff
								copy(c.rbuf[:rem], c.rbuf[c.roff:c.rlen])
								c.roff = 0
								c.rlen = rem
							}
						} else if c.rlen == len(c.rbuf) {
							if c.roff > 0 {
								rem := c.rlen - c.roff
								copy(c.rbuf[:rem], c.rbuf[c.roff:c.rlen])
								c.roff = 0
								c.rlen = rem
							} else {
								grown := make([]byte, len(c.rbuf)*2)
								copy(grown, c.rbuf[:c.rlen])
								c.rbuf = grown
							}
						}

						nr, rErr := syscall.Read(c.fd, c.rbuf[c.rlen:])
						if nr > 0 {
							c.rlen += nr
						} else if nr == 0 {
							sawEOF = true
						} else if rErr == syscall.EINTR {
							continue
						} else if rErr != syscall.EAGAIN && rErr != syscall.EWOULDBLOCK {
							connClosed = true
							break
						}

						parsedComplete := false
						needMore := false
						for c.roff < c.rlen {
							req, consumed, hint, ns, st := parseHTTPRequestInPlace(c.rbuf[c.roff:c.rlen], c.chunkSc, c.sent100)
							c.chunkSc = ns
							if st == parseSend100 {
								c.roff += consumed
								if !writeResponse(c, expect100Msg) {
									connClosed = true
									break
								}
								c.sent100 = true
								continue
							}
							if st == parseNeedMore {
								c.roff += consumed
								needHint = hint
								needMore = true
								break
							}
							if st == parseBad {
								connClosed = true
								break
							}

							c.roff += consumed
							c.sent100 = false
							needHint = 0
							parsedComplete = true

							resp := dispatchRequest(&req, store, expectedAuth)
							if !writeResponse(c, resp) {
								connClosed = true
								break
							}
							if req.CloseConn {
								if len(c.wbuf) > c.woff {
									c.closing = true
									_ = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_MOD, c.fd, &syscall.EpollEvent{
										Events: syscall.EPOLLOUT,
										Fd:     int32(c.fd),
									})
								} else {
									connClosed = true
								}
								break
							}
						}

						if connClosed || c.closing {
							break
						}

						if c.roff == c.rlen {
							c.roff = 0
							c.rlen = 0
							if cap(c.rbuf) > maxPinnedConnBuf {
								c.rbuf = make([]byte, defaultConnRBuf)
							}
							if parsedComplete {
								if sawRDHUP || sawEOF {
									if len(c.wbuf) > c.woff {
										c.closing = true
										_ = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_MOD, c.fd, &syscall.EpollEvent{
											Events: syscall.EPOLLOUT,
											Fd:     int32(c.fd),
										})
									} else {
										connClosed = true
									}
								}
								// All bytes consumed on this readable event: wait for next epoll event
								// instead of issuing a speculative EAGAIN read() syscall.
								break
							}
						}

						if sawEOF {
							if needMore || c.rlen == 0 {
								if len(c.wbuf) > c.woff && c.rlen == 0 {
									c.closing = true
									_ = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_MOD, c.fd, &syscall.EpollEvent{
										Events: syscall.EPOLLOUT,
										Fd:     int32(c.fd),
									})
								} else {
									connClosed = true
								}
							}
							break
						}
						if !needMore && nr <= 0 {
							break
						}
						if rErr == syscall.EAGAIN || rErr == syscall.EWOULDBLOCK {
							break
						}
					}

					if connClosed || (sawRDHUP && !c.closing && c.rlen == 0) {
						if len(c.wbuf) > c.woff && !connClosed {
							c.closing = true
							_ = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_MOD, c.fd, &syscall.EpollEvent{
								Events: syscall.EPOLLOUT,
								Fd:     int32(c.fd),
							})
						} else {
							closeConn(c)
						}
					}
				}
			}
		}
	}

	return port, run, shutdown, nil
}

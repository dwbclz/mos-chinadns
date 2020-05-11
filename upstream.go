//     Copyright (C) 2020, IrineSistiana
//
//     This file is part of mos-chinadns.
//
//     mos-chinadns is free software: you can redistribute it and/or modify
//     it under the terms of the GNU General Public License as published by
//     the Free Software Foundation, either version 3 of the License, or
//     (at your option) any later version.
//
//     mos-chinadns is distributed in the hope that it will be useful,
//     but WITHOUT ANY WARRANTY; without even the implied warranty of
//     MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//     GNU General Public License for more details.
//
//     You should have received a copy of the GNU General Public License
//     along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/IrineSistiana/mos-chinadns/utils"

	"github.com/IrineSistiana/mos-chinadns/bufpool"
	"github.com/IrineSistiana/mos-chinadns/dohclient"
	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

type upstream interface {
	Exchange(ctx context.Context, qRaw []byte, requestLogger *logrus.Entry) (rRaw []byte, rtt time.Duration, err error)
}

type upstreamTCP struct {
	addr string
}

type upstreamUDP struct {
	addr       *net.UDPAddr
	maxUDPSize int
	cp         *udpConnPool
}

func newUpstream(addr, prot, url string, rootCAs *x509.CertPool) (upstream, error) {
	var client upstream
	switch prot {
	case "tcp":
		client = &upstreamTCP{
			addr: addr,
		}
	case "udp", "":
		udpAddr, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			return nil, err
		}
		client = &upstreamUDP{
			addr:       udpAddr,
			maxUDPSize: 1480,
			cp:         newUDPConnPool(0xffff, time.Second*10, time.Second*10),
		}
	case "doh":
		tlsConf := &tls.Config{
			// don't have to set servername here, fasthttp will do it itself.
			RootCAs:            rootCAs,
			ClientSessionCache: tls.NewLRUClientSessionCache(64),
		}

		if len(url) == 0 {
			return nil, fmt.Errorf("protocol [%s] needs URL", prot)
		}
		client = dohclient.NewClient(url, addr, tlsConf, dns.MaxMsgSize, queryTimeout)
	default:
		return nil, fmt.Errorf("unsupport protocol: %s", prot)
	}

	return client, nil
}

func (u *upstreamTCP) Exchange(ctx context.Context, qRaw []byte, _ *logrus.Entry) (rRaw []byte, rtt time.Duration, err error) {
	t := time.Now()
	r, err := u.exchange(ctx, qRaw)
	return r, time.Since(t), err
}

func (u *upstreamTCP) exchange(ctx context.Context, qRaw []byte) (rRaw []byte, err error) {
	d := net.Dialer{}
	c, err := d.DialContext(ctx, "tcp", u.addr)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	go func() {
		select {
		case <-ctx.Done():
			c.SetDeadline(time.Now())
		}
	}()

	err = writeMsgToTCP(c, qRaw)
	if err != nil {
		return nil, err
	}
	rRaw, err = readMsgFromTCP(c)
	if err != nil {
		return nil, err
	}

	msgID := utils.GetMsgID(qRaw)
	if utils.GetMsgID(rRaw) != msgID {
		bufpool.ReleaseMsgBuf(rRaw)
		return nil, dns.ErrId
	}
	return rRaw, nil
}

func (u *upstreamUDP) Exchange(ctx context.Context, qRaw []byte, _ *logrus.Entry) (rRaw []byte, rtt time.Duration, err error) {
	t := time.Now()
	rRaw, err = u.exchange(ctx, qRaw)
	return rRaw, time.Since(t), err
}

func (u *upstreamUDP) exchange(ctx context.Context, qRaw []byte) (rRaw []byte, err error) {

	var isNewConn bool
	c := u.cp.get()
	if c == nil {
		c, err = net.DialUDP("udp", nil, u.addr)
		if err != nil {
			return nil, err
		}
		isNewConn = true
	}
	c.SetDeadline(time.Time{})

	once := sync.Once{}
	go func() {
		select {
		case <-ctx.Done():
			once.Do(func() { c.SetDeadline(time.Now()) })
		}
	}()

	_, err = c.Write(qRaw)
	if err != nil {
		c.Close()
		return nil, err
	}

	buf := bufpool.AcquireMsgBuf(u.maxUDPSize)
read:
	n, err := c.Read(buf)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() && ctx.Err() != nil {
			// err caused by cancelled ctx, it's ok to reuse the connection
			once.Do(func() {})
			u.cp.put(c)
			bufpool.ReleaseMsgBuf(buf)
			return nil, err
		}
		c.Close()
		bufpool.ReleaseMsgBuf(buf)
		return nil, err
	}

	if n < 12 {
		err = dns.ErrShortRead
		c.Close()
		bufpool.ReleaseMsgBuf(buf)
		return nil, err
	}

	rRaw = buf[:n]
	if utils.GetMsgID(rRaw) != utils.GetMsgID(qRaw) && !isNewConn {
		// this connection is reused, rRaw might be the reply
		// of last qRaw, not this qRaw.
		// try to read again
		goto read
	}

	once.Do(func() {})
	u.cp.put(c)
	return rRaw, nil
}

type udpConnPool struct {
	sync.Mutex
	maxSize          int
	pool             []udpConnPoolElem
	ttl              time.Duration
	lastClean        time.Time
	cleannerInterval time.Duration
}

type udpConnPoolElem struct {
	*net.UDPConn
	lastUsed time.Time
}

func newUDPConnPool(size int, ttl, gcInterval time.Duration) *udpConnPool {
	return &udpConnPool{
		maxSize:          size,
		pool:             make([]udpConnPoolElem, 0, 64),
		ttl:              ttl,
		cleannerInterval: gcInterval,
	}

}

// runCleanner must run under lock
func (p *udpConnPool) runCleanner() {
	if p == nil && len(p.pool) == 0 {
		return
	}

	//scheduled
	if time.Since(p.lastClean) > p.cleannerInterval {
		p.lastClean = time.Now()
		res := p.pool[:0]
		for i := range p.pool {

			// remove expired conns
			if time.Since(p.pool[i].lastUsed) < p.ttl {
				res = append(res, p.pool[i])
			} else { // expired, release the resources
				p.pool[i].UDPConn.Close()
				p.pool[i].UDPConn = nil
			}
		}
		p.pool = res
	}

	//when the pool is full
	if len(p.pool) >= p.maxSize {
		res := p.pool[:0]
		mid := len(p.pool) >> 1
		for i := range p.pool {
			// forcely remove half conns first
			if i < mid {
				p.pool[i].UDPConn.Close()
				p.pool[i].UDPConn = nil
			}

			//then remove expired conns
			if time.Since(p.pool[i].lastUsed) < p.ttl {
				res = append(res, p.pool[i])
			} else {
				p.pool[i].UDPConn.Close()
				p.pool[i].UDPConn = nil
			}
		}
		p.pool = res
	}
}

func (p *udpConnPool) put(c *net.UDPConn) {
	if p == nil && p.maxSize <= 0 {
		return
	}

	p.Lock()
	defer p.Unlock()

	p.runCleanner()

	if len(p.pool) >= p.maxSize {
		c.Close() // pool is full, drop it
	} else {
		p.pool = append(p.pool, udpConnPoolElem{UDPConn: c, lastUsed: time.Now()})
	}
}

func (p *udpConnPool) get() (c *net.UDPConn) {
	if p == nil && p.maxSize <= 0 {
		return nil
	}

	p.Lock()
	defer p.Unlock()

	p.runCleanner()

	if len(p.pool) > 0 {
		e := p.pool[len(p.pool)-1]
		p.pool = p.pool[:len(p.pool)-1]

		if time.Since(e.lastUsed) > p.ttl {
			e.UDPConn.Close() // expired
			return nil
		}
		return e.UDPConn
	}
	return nil
}
package dnsPoison

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcapgo"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"
	"log"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	v2router "v2ray.com/core/app/router"
	"v2ray.com/core/common/net"
	"v2ray.com/core/common/strmatcher"
)

type handle struct {
	done    chan interface{}
	running bool
	*pcapgo.EthernetHandle
	inspectedWhiteDomains map[string]interface{}
	domainMutex           sync.RWMutex
}

func newHandle(ethernetHandle *pcapgo.EthernetHandle) handle {
	return handle{
		done:                  make(chan interface{}),
		EthernetHandle:        ethernetHandle,
		inspectedWhiteDomains: make(map[string]interface{}),
	}
}

type DnsPoison struct {
	handles map[string]handle
	reqID   uint32
	inner   sync.Mutex
}

func New() *DnsPoison {
	return &DnsPoison{
		handles: make(map[string]handle),
	}
}

func (d *DnsPoison) Exists(ifname string) bool {
	_, ok := d.handles[ifname]
	return ok
}

func (d *DnsPoison) Clear() {
	log.Println("DnsPoison: Clear")
	handles := d.ListHandles()
	for _, h := range handles {
		_ = d.DeleteHandles(h)
	}
}

func (d *DnsPoison) Prepare(ifname string) (err error) {
	d.inner.Lock()
	defer d.inner.Unlock()
	if d.Exists(ifname) {
		return errors.New(ifname + " exists")
	}
	h, err := pcapgo.NewEthernetHandle(ifname)
	if err != nil {
		return
	}
	d.handles[ifname] = newHandle(h)
	return
}

func (d *DnsPoison) ListHandles() (ifnames []string) {
	d.inner.Lock()
	defer d.inner.Unlock()
	for ifname := range d.handles {
		ifnames = append(ifnames, ifname)
	}
	return
}

func (d *DnsPoison) DeleteHandles(ifname string) (err error) {
	d.inner.Lock()
	defer d.inner.Unlock()
	if !d.Exists(ifname) {
		return errors.New("handle not exists")
	}
	close(d.handles[ifname].done)
	delete(d.handles, ifname)
	return
}

func bindAddr(fd uintptr, ip []byte, port uint32) error {
	if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return errors.New("failed to set resuse_addr")
	}

	if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		return errors.New("failed to set resuse_port")
	}

	var sockaddr syscall.Sockaddr

	switch len(ip) {
	case net.IPv4len:
		a4 := &syscall.SockaddrInet4{
			Port: int(port),
		}
		copy(a4.Addr[:], ip)
		sockaddr = a4
	case net.IPv6len:
		a6 := &syscall.SockaddrInet6{
			Port: int(port),
		}
		copy(a6.Addr[:], ip)
		sockaddr = a6
	default:
		return errors.New("unexpected length of ip")
	}

	return syscall.Bind(int(fd), sockaddr)
}
func Fqdn(domain string) string {
	if len(domain) > 0 && domain[len(domain)-1] == '.' {
		return domain
	}
	return domain + "."
}

func (d *DnsPoison) Run(ifname string, whitelistDnsServers *v2router.GeoIPMatcher, whitelistDomains *strmatcher.MatcherGroup) (err error) {
	d.inner.Lock()
	handle, ok := d.handles[ifname]
	if !ok {
		return errors.New(ifname + " not exsits")
	}
	if handle.running {
		return errors.New(ifname + " is running")
	}
	handle.running = true
	d.inner.Unlock()
	log.Println("DnsPoison[" + ifname + "]: running")
	pkgsrc := gopacket.NewPacketSource(handle, layers.LayerTypeEthernet)
	pkgsrc.NoCopy = true
out:
	for packet := range pkgsrc.Packets() {
		select {
		case <-handle.done:
			log.Println("DnsPoison:", ifname, "closed")
			break out
		default:
		}
		trans := packet.TransportLayer()
		if trans == nil {
			continue
		}
		transflow := trans.TransportFlow()
		sPort, dPort := transflow.Endpoints()
		if dPort.String() != "53" && sPort.String() != "53" {
			continue
		}
		sAddr, dAddr := packet.NetworkLayer().NetworkFlow().Endpoints()
		// TODO: 暂不支持IPv6
		sIp := net.ParseIP(sAddr.String()).To4()
		if len(sIp) != net.IPv4len {
			continue
		}
		// whitelistIps
		if ok := whitelistDnsServers.Match(sIp); ok {
			continue
		}
		dIp := net.ParseIP(dAddr.String()).To4()
		if len(dIp) != net.IPv4len {
			continue
		}
		// whitelistIps
		if ok := whitelistDnsServers.Match(dIp); ok {
			continue
		}
		var m dnsmessage.Message
		err := m.Unpack(trans.LayerPayload())
		if err != nil {
			continue
		}
		// dns请求一般只有一个question
		q := m.Questions[0]
		if (q.Type != dnsmessage.TypeA && q.Type != dnsmessage.TypeAAAA) ||
			q.Class != dnsmessage.ClassINET {
			continue
		}
		// whitelistDomains
		dm := q.Name.String()
		if len(dm) == 0 {
			continue
		} else if index := whitelistDomains.Match(dm[:len(dm)-1]); index > 0 {
			continue
		}
		if !m.Response {
			if q.Type == dnsmessage.TypeA {
				//在已探测白名单中的放行
				handle.domainMutex.RLock()
				if _, ok := handle.inspectedWhiteDomains[q.Name.String()]; ok {
					handle.domainMutex.RUnlock()
					continue
				}
				handle.domainMutex.RUnlock()
				log.Println("dnsPoison["+ifname+"]:", sAddr.String()+":"+sPort.String(), "->", dAddr.String()+":"+dPort.String(), dm, "已投毒")
			}
			//TODO: 不支持IPv6，AAAA投毒返回空，加速解析
			go poison(&m, &dAddr, &dPort, &sAddr, &sPort)
		} else {
			//探测该域名是否被污染为本地回环
			spooffed := false
			var msgs []string
			for _, a := range m.Answers {
				switch a := a.Body.(type) {
				case *dnsmessage.CNAMEResource:
					msgs = append(msgs, "CNAME:"+strings.TrimSuffix(a.CNAME.String(), "."))
				case *dnsmessage.AResource:
					msgs = append(msgs, "A:"+fmt.Sprintf("%v.%v.%v.%v", a.A[0], a.A[1], a.A[2], a.A[3]))
					if bytes.Equal(a.A[:], []byte{127, 0, 0, 1}) {
						spooffed = true
					}
				}
			}
			msg := strings.Join(msgs, ", ")
			if msg == "" {
				msg = "(empty)"
			} else {
				msg = "[" + msg + "]"
			}
			log.Println("dnsPoison["+ifname+"]:", dAddr.String()+":"+dPort.String(), "<-", sAddr.String()+":"+sPort.String(), dm, "=>", msg)
			if spooffed {
				handle.domainMutex.RLock()
				if _, ok := handle.inspectedWhiteDomains[dm]; ok {
					handle.domainMutex.RUnlock()
					handle.domainMutex.Lock()
					delete(handle.inspectedWhiteDomains, dm)
					handle.domainMutex.Unlock()
					log.Println("dnsPoison["+ifname+"]: 探测到", dm, "从白名单移除", msg)
				} else {
					handle.domainMutex.RUnlock()
				}
			} else {
				//返回的是非本地回环IP
				handle.domainMutex.RLock()
				if _, ok := handle.inspectedWhiteDomains[dm]; !ok {
					handle.domainMutex.RUnlock()
					handle.domainMutex.Lock()
					handle.inspectedWhiteDomains[dm] = nil
					handle.domainMutex.Unlock()
					log.Println("dnsPoison["+ifname+"]: 探测到", dm, "加入白名单", msg)
				} else {
					handle.domainMutex.RUnlock()
				}
			}
		}
	}
	return
}

func poison(m *dnsmessage.Message, lAddr, lPort, rAddr, rPort *gopacket.Endpoint) {
	q := m.Questions[0]
	m.RCode = dnsmessage.RCodeSuccess
	switch q.Type {
	case dnsmessage.TypeAAAA:
		//返回空回答
	case dnsmessage.TypeA:
		//对A查询返回一个公网地址以使得后续tcp连接经过网关嗅探，以dns污染解决dns污染
		m.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{
				Name:  q.Name,
				Type:  q.Type,
				Class: q.Class,
				TTL:   0,
			},
			Body: &dnsmessage.AResource{A: [4]byte{1, 2, 3, 4}},
		}}
	}
	m.Response = true
	m.RecursionAvailable = true
	packed, _ := m.Pack()
	// write back
	lport, _ := strconv.Atoi(lPort.String())
	conn, err := newDialer(lAddr.String(), uint32(lport), 30*time.Second).Dial("udp", rAddr.String()+":"+rPort.String())
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write(packed)
}

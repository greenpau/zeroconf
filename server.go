package bonjour

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const (
	// Number of Multicast responses sent for a query message (default: 1 < x < 9)
	multicastRepitions = 1
)

// Register a service by given arguments. This call will take the system's hostname
// and lookup IP by that hostname.
func Register(instance, service, domain string, port int, text []string, iface *net.Interface) (*Server, error) {
	entry := NewServiceEntry(instance, service, domain)
	entry.Port = port
	entry.Text = text

	if entry.Instance == "" {
		return nil, fmt.Errorf("Missing service instance name")
	}
	if entry.Service == "" {
		return nil, fmt.Errorf("Missing service name")
	}
	if entry.Domain == "" {
		entry.Domain = "local"
	}
	if entry.Port == 0 {
		return nil, fmt.Errorf("Missing port")
	}

	var err error
	if entry.HostName == "" {
		entry.HostName, err = os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("Could not determine host")
		}
	}

	if !strings.HasSuffix(trimDot(entry.HostName), entry.Domain) {
		entry.HostName = fmt.Sprintf("%s.%s.", trimDot(entry.HostName), trimDot(entry.Domain))
	}

	// GetLocalIP returns the non loopback local IP of the host
	iaddrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}

	for _, address := range iaddrs {
		// check the address type and if it is not a loopback the display it
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				entry.AddrIPv4 = ipnet.IP
			} else if ipnet.IP.To16() != nil {
				// Favor global instead of local IP scope
				if entry.AddrIPv6 != nil {
					if ipnet.IP.IsGlobalUnicast() {
						entry.AddrIPv6 = ipnet.IP
						log.Printf("Added global IPv6: %v\n", entry.AddrIPv6)
					}

				} else {
					entry.AddrIPv6 = ipnet.IP
				}
			}
		}
	}

	if entry.AddrIPv4 == nil && entry.AddrIPv6 == nil {
		return nil, fmt.Errorf("Could not determine host IP addresses")
	}

	s, err := newServer(iface)
	if err != nil {
		return nil, err
	}

	s.service = entry
	go s.mainloop()
	go s.probe()

	return s, nil
}

// Register a service proxy by given argument. This call will skip the hostname/IP lookup and
// will use the provided values.
func RegisterProxy(instance, service, domain string, port int, host, ip string, text []string, iface *net.Interface) (*Server, error) {
	entry := NewServiceEntry(instance, service, domain)
	entry.Port = port
	entry.Text = text
	entry.HostName = host

	if entry.Instance == "" {
		return nil, fmt.Errorf("Missing service instance name")
	}
	if entry.Service == "" {
		return nil, fmt.Errorf("Missing service name")
	}
	if entry.HostName == "" {
		return nil, fmt.Errorf("Missing host name")
	}
	if entry.Domain == "" {
		entry.Domain = "local"
	}
	if entry.Port == 0 {
		return nil, fmt.Errorf("Missing port")
	}

	if !strings.HasSuffix(trimDot(entry.HostName), entry.Domain) {
		entry.HostName = fmt.Sprintf("%s.%s.", trimDot(entry.HostName), trimDot(entry.Domain))
	}

	ipAddr := net.ParseIP(ip)
	if ipAddr == nil {
		return nil, fmt.Errorf("Failed to parse given IP: %v", ip)
	} else if ipv4 := ipAddr.To4(); ipv4 != nil {
		entry.AddrIPv4 = ipAddr
	} else if ipv6 := ipAddr.To16(); ipv6 != nil {
		entry.AddrIPv6 = ipAddr
	} else {
		return nil, fmt.Errorf("The IP is neither IPv4 nor IPv6: %#v", ipAddr)
	}

	s, err := newServer(iface)
	if err != nil {
		return nil, err
	}

	s.service = entry
	go s.mainloop()
	go s.probe()

	return s, nil
}

// Server structure incapsulates both IPv4/IPv6 UDP connections
type Server struct {
	service        *ServiceEntry
	ipv4conn       *net.UDPConn
	ipv6conn       *net.UDPConn
	shouldShutdown bool
	shutdownLock   sync.Mutex
	ttl            uint32
}

// Constructs server structure
func newServer(iface *net.Interface) (*Server, error) {
	ipv4conn, ipv6conn, err := newConnection(iface)
	if err != nil {
		return nil, err
	}

	s := &Server{
		ipv4conn: ipv4conn,
		ipv6conn: ipv6conn,
		ttl:      3200,
	}

	return s, nil
}

// Start listeners and waits for the shutdown signal from exit channel
func (s *Server) mainloop() {
	if s.ipv4conn != nil {
		go s.recv(s.ipv4conn)
	}
	if s.ipv6conn != nil {
		go s.recv(s.ipv6conn)
	}
}

// Shutdown closes all udp connections and unregisters the service
func (s *Server) Shutdown() {
	s.shutdown()
}

// SetText updates and announces the TXT records
func (s *Server) SetText(text []string) {
	s.service.Text = text
	s.announceText()
}

// TTL sets the TTL for DNS replies
func (s *Server) TTL(ttl uint32) {
	s.ttl = ttl
}

// Shutdown server will close currently open connections & channel
func (s *Server) shutdown() error {
	s.shutdownLock.Lock()
	defer s.shutdownLock.Unlock()

	s.unregister()

	if s.shouldShutdown {
		return nil
	}
	s.shouldShutdown = true

	if s.ipv4conn != nil {
		s.ipv4conn.Close()
	}
	if s.ipv6conn != nil {
		s.ipv6conn.Close()
	}
	return nil
}

// recv is a long running routine to receive packets from an interface
func (s *Server) recv(c *net.UDPConn) {
	if c == nil {
		return
	}
	buf := make([]byte, 65536)
	for !s.shouldShutdown {
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			continue
		}
		if err := s.parsePacket(buf[:n], from); err != nil {
			log.Printf("[ERR] bonjour: Failed to handle query: %v", err)
		}
	}
}

// parsePacket is used to parse an incoming packet
func (s *Server) parsePacket(packet []byte, from net.Addr) error {
	var msg dns.Msg
	if err := msg.Unpack(packet); err != nil {
		log.Printf("[ERR] bonjour: Failed to unpack packet: %v", err)
		return err
	}
	return s.handleQuery(&msg, from)
}

// handleQuery is used to handle an incoming query
func (s *Server) handleQuery(query *dns.Msg, from net.Addr) error {
	// Ignore answer for now
	if len(query.Answer) > 0 {
		return nil
	}
	// Ignore questions with Authorative section for now
	if len(query.Ns) > 0 {
		return nil
	}

	// Handle each question
	var (
		resp dns.Msg
		err  error
	)
	if len(query.Question) > 0 {
		for _, q := range query.Question {
			resp = dns.Msg{}
			resp.SetReply(query)
			resp.Answer = []dns.RR{}
			resp.Extra = []dns.RR{}
			if err = s.handleQuestion(q, &resp); err != nil {
				log.Printf("[ERR] bonjour: failed to handle question %v: %v",
					q, err)
				continue
			}
			// Check if there is an answer
			if len(resp.Answer) > 0 {
				if isUnicastQuestion(q) {
					// Send unicast
					if e := s.unicastResponse(&resp, from); e != nil {
						err = e
					}
				} else {
					// Send mulicast
					if e := s.multicastResponse(&resp); e != nil {
						err = e
					}
				}
			}
		}
	}

	return err
}

// handleQuestion is used to handle an incoming question
func (s *Server) handleQuestion(q dns.Question, resp *dns.Msg) error {
	if s.service == nil {
		return nil
	}

	switch q.Name {
	case s.service.ServiceName():
		s.composeBrowsingAnswers(resp, s.ttl)
	case s.service.ServiceInstanceName():
		s.composeLookupAnswers(resp, s.ttl)
	case s.service.ServiceTypeName():
		s.serviceTypeName(resp, s.ttl)
	}

	return nil
}

func (s *Server) composeBrowsingAnswers(resp *dns.Msg, ttl uint32) {
	ptr := &dns.PTR{
		Hdr: dns.RR_Header{
			Name:   s.service.ServiceName(),
			Rrtype: dns.TypePTR,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		},
		Ptr: s.service.ServiceInstanceName(),
	}
	resp.Answer = append(resp.Answer, ptr)

	txt := &dns.TXT{
		Hdr: dns.RR_Header{
			Name:   s.service.ServiceInstanceName(),
			Rrtype: dns.TypeTXT,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		},
		Txt: s.service.Text,
	}
	srv := &dns.SRV{
		Hdr: dns.RR_Header{
			Name:   s.service.ServiceInstanceName(),
			Rrtype: dns.TypeSRV,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		},
		Priority: 0,
		Weight:   0,
		Port:     uint16(s.service.Port),
		Target:   s.service.HostName,
	}
	resp.Extra = append(resp.Extra, srv, txt)

	if s.service.AddrIPv4 != nil {
		a := &dns.A{
			Hdr: dns.RR_Header{
				Name:   s.service.HostName,
				Rrtype: dns.TypeA,
				Class:  dns.ClassINET,
				Ttl:    ttl,
			},
			A: s.service.AddrIPv4,
		}
		resp.Extra = append(resp.Extra, a)
	}
	if s.service.AddrIPv6 != nil {
		aaaa := &dns.AAAA{
			Hdr: dns.RR_Header{
				Name:   s.service.HostName,
				Rrtype: dns.TypeAAAA,
				Class:  dns.ClassINET,
				Ttl:    ttl,
			},
			AAAA: s.service.AddrIPv6,
		}
		resp.Extra = append(resp.Extra, aaaa)
	}
}

func (s *Server) composeLookupAnswers(resp *dns.Msg, ttl uint32) {
	// From RFC6762
	//    The most significant bit of the rrclass for a record in the Answer
	//    Section of a response message is the Multicast DNS cache-flush bit
	//    and is discussed in more detail below in Section 10.2, "Announcements
	//    to Flush Outdated Cache Entries".
	cache_flush := uint16(1 << 15)
	ptr := &dns.PTR{
		Hdr: dns.RR_Header{
			Name:   s.service.ServiceName(),
			Rrtype: dns.TypePTR,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		},
		Ptr: s.service.ServiceInstanceName(),
	}
	srv := &dns.SRV{
		Hdr: dns.RR_Header{
			Name:   s.service.ServiceInstanceName(),
			Rrtype: dns.TypeSRV,
			Class:  dns.ClassINET | cache_flush,
			Ttl:    ttl,
		},
		Priority: 0,
		Weight:   0,
		Port:     uint16(s.service.Port),
		Target:   s.service.HostName,
	}
	txt := &dns.TXT{
		Hdr: dns.RR_Header{
			Name:   s.service.ServiceInstanceName(),
			Rrtype: dns.TypeTXT,
			Class:  dns.ClassINET | cache_flush,
			Ttl:    ttl,
		},
		Txt: s.service.Text,
	}
	dnssd := &dns.PTR{
		Hdr: dns.RR_Header{
			Name:   s.service.ServiceTypeName(),
			Rrtype: dns.TypePTR,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		},
		Ptr: s.service.ServiceName(),
	}
	resp.Answer = append(resp.Answer, srv, txt, ptr, dnssd)

	if s.service.AddrIPv4 != nil {
		a := &dns.A{
			Hdr: dns.RR_Header{
				Name:   s.service.HostName,
				Rrtype: dns.TypeA,
				Class:  dns.ClassINET | cache_flush,
				Ttl:    120,
			},
			A: s.service.AddrIPv4,
		}
		resp.Answer = append(resp.Answer, a)
	}
	if s.service.AddrIPv6 != nil {
		aaaa := &dns.AAAA{
			Hdr: dns.RR_Header{
				Name:   s.service.HostName,
				Rrtype: dns.TypeAAAA,
				Class:  dns.ClassINET | cache_flush,
				Ttl:    120,
			},
			AAAA: s.service.AddrIPv6,
		}
		resp.Answer = append(resp.Answer, aaaa)
	}
}

func (s *Server) serviceTypeName(resp *dns.Msg, ttl uint32) {
	// From RFC6762
	// 9.  Service Type Enumeration
	//
	//    For this purpose, a special meta-query is defined.  A DNS query for
	//    PTR records with the name "_services._dns-sd._udp.<Domain>" yields a
	//    set of PTR records, where the rdata of each PTR record is the two-
	//    label <Service> name, plus the same domain, e.g.,
	//    "_http._tcp.<Domain>".
	dnssd := &dns.PTR{
		Hdr: dns.RR_Header{
			Name:   s.service.ServiceTypeName(),
			Rrtype: dns.TypePTR,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		},
		Ptr: s.service.ServiceName(),
	}
	resp.Answer = append(resp.Answer, dnssd)
}

// Perform probing & announcement
//TODO: implement a proper probing & conflict resolution
func (s *Server) probe() {
	q := new(dns.Msg)
	q.SetQuestion(s.service.ServiceInstanceName(), dns.TypePTR)
	q.RecursionDesired = false

	srv := &dns.SRV{
		Hdr: dns.RR_Header{
			Name:   s.service.ServiceInstanceName(),
			Rrtype: dns.TypeSRV,
			Class:  dns.ClassINET,
			Ttl:    s.ttl,
		},
		Priority: 0,
		Weight:   0,
		Port:     uint16(s.service.Port),
		Target:   s.service.HostName,
	}
	txt := &dns.TXT{
		Hdr: dns.RR_Header{
			Name:   s.service.ServiceInstanceName(),
			Rrtype: dns.TypeTXT,
			Class:  dns.ClassINET,
			Ttl:    s.ttl,
		},
		Txt: s.service.Text,
	}
	q.Ns = []dns.RR{srv, txt}

	randomizer := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < multicastRepitions; i++ {
		if err := s.multicastResponse(q); err != nil {
			log.Println("[ERR] bonjour: failed to send probe:", err.Error())
		}
		time.Sleep(time.Duration(randomizer.Intn(250)) * time.Millisecond)
	}
	resp := new(dns.Msg)
	resp.MsgHdr.Response = true
	// TODO: make response authoritative if we are the publisher
	resp.Answer = []dns.RR{}
	resp.Extra = []dns.RR{}
	s.composeLookupAnswers(resp, s.ttl)

	// From RFC6762
	//    The Multicast DNS responder MUST send at least two unsolicited
	//    responses, one second apart. To provide increased robustness against
	//    packet loss, a responder MAY send up to eight unsolicited responses,
	//    provided that the interval between unsolicited responses increases by
	//    at least a factor of two with every response sent.
	timeout := 1 * time.Second
	for i := 0; i < multicastRepitions; i++ {
		if err := s.multicastResponse(resp); err != nil {
			log.Println("[ERR] bonjour: failed to send announcement:", err.Error())
		}
		time.Sleep(timeout)
		timeout *= 2
	}
}

// announceText sends a Text announcement with cache flush enabled
func (s *Server) announceText() {
	resp := new(dns.Msg)
	resp.MsgHdr.Response = true

	txt := &dns.TXT{
		Hdr: dns.RR_Header{
			Name:   s.service.ServiceInstanceName(),
			Rrtype: dns.TypeTXT,
			Class:  dns.ClassINET | 1<<15,
			Ttl:    s.ttl,
		},
		Txt: s.service.Text,
	}

	resp.Answer = []dns.RR{txt}
	s.multicastResponse(resp)
}

func (s *Server) unregister() error {
	resp := new(dns.Msg)
	resp.MsgHdr.Response = true
	resp.Answer = []dns.RR{}
	resp.Extra = []dns.RR{}
	s.composeLookupAnswers(resp, 0)
	return s.multicastResponse(resp)
}

// unicastResponse is used to send a unicast response packet
func (s *Server) unicastResponse(resp *dns.Msg, from net.Addr) error {
	buf, err := resp.Pack()
	if err != nil {
		return err
	}
	addr := from.(*net.UDPAddr)
	if addr.IP.To4() != nil {
		_, err = s.ipv4conn.WriteToUDP(buf, addr)
		return err
	} else {
		_, err = s.ipv6conn.WriteToUDP(buf, addr)
		return err
	}
}

// multicastResponse us used to send a multicast response packet
func (s *Server) multicastResponse(msg *dns.Msg) error {
	buf, err := msg.Pack()
	if err != nil {
		log.Println("Failed to pack message!")
		return err
	}
	if s.ipv4conn != nil {
		s.ipv4conn.WriteTo(buf, ipv4Addr)
	}
	if s.ipv6conn != nil {
		s.ipv6conn.WriteTo(buf, ipv6Addr)
	}
	return nil
}

func isUnicastQuestion(q dns.Question) bool {
	// From RFC6762
	// 18.12.  Repurposing of Top Bit of qclass in Question Section
	//
	//    In the Question Section of a Multicast DNS query, the top bit of the
	//    qclass field is used to indicate that unicast responses are preferred
	//    for this particular question.  (See Section 5.4.)
	return q.Qclass&(1<<15) != 0
}

package spf

import (
	"net"
	"sync"

	"github.com/miekg/dns"
)

// NewMiekgDNSResolver returns new instance of Resolver
func NewMiekgDNSResolver(addr string) Resolver {
	return &MiekgDNSResolver{
		client:     new(dns.Client),
		serverAddr: addr,
	}
}

// MiekgDNSResolver implements Resolver using github.com/miekg/dns
type MiekgDNSResolver struct {
	mu         sync.Mutex
	client     *dns.Client
	serverAddr string
}

// If the DNS lookup returns a server failure (RCODE 2) or some other
// error (RCODE other than 0 or 3), or if the lookup times out, then
// check_host() terminates immediately with the result "temperror".
func (r *MiekgDNSResolver) exchange(req *dns.Msg) (*dns.Msg, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, _, err := r.client.Exchange(req, r.serverAddr)
	if err != nil {
		return nil, ErrDNSTemperror
	}
	if res.Rcode == dns.RcodeNameError {
		return nil, ErrDNSPermerror
	}
	if res.Rcode != dns.RcodeSuccess {
		return nil, ErrDNSTemperror
	}
	return res, nil
}

// LookupTXT returns the DNS TXT records for the given domain name.
func (r *MiekgDNSResolver) LookupTXT(name string) ([]string, error) {
	req := new(dns.Msg)
	req.SetQuestion(name, dns.TypeTXT)

	res, err := r.exchange(req)
	if err != nil {
		return nil, err
	}

	txts := make([]string, 0, len(res.Answer))
	for _, a := range res.Answer {
		if r, ok := a.(*dns.TXT); ok {
			txts = append(txts, r.Txt...)
		}
	}
	return txts, nil
}

// Exists is used for a DNS A RR lookup (even when the
// connection type is IPv6).  If any A record is returned, this
// mechanism matches.
func (r *MiekgDNSResolver) Exists(name string) (bool, error) {
	req := new(dns.Msg)
	req.SetQuestion(name, dns.TypeA)

	res, err := r.exchange(req)
	if err != nil {
		return false, err
	}

	return len(res.Answer) > 0, nil
}

func matchIP(rrs []dns.RR, matcher IPMatcherFunc) (bool, error) {
	for _, rr := range rrs {
		var ip net.IP
		switch a := rr.(type) {
		case *dns.A:
			ip = a.A
		case *dns.AAAA:
			ip = a.AAAA
		}
		if m, e := matcher(ip); m || e != nil {
			return m, e
		}
	}
	return false, nil
}

// MatchIP provides an address lookup, which should be done on the name
// using the type of lookup (A or AAAA).
// Then IPMatcherFunc used to compare checked IP to the returned address(es).
// If any address matches, the mechanism matches
func (r *MiekgDNSResolver) MatchIP(name string, matcher IPMatcherFunc) (bool, error) {
	var wg sync.WaitGroup
	hits := make(chan hit)

	for _, qType := range []uint16{dns.TypeA, dns.TypeAAAA} {
		wg.Add(1)
		go func(qType uint16) {
			defer wg.Done()

			req := new(dns.Msg)
			req.SetQuestion(name, qType)
			res, err := r.exchange(req)
			if err != nil {
				hits <- hit{false, err}
				return
			}

			if m, e := matchIP(res.Answer, matcher); m || e != nil {
				hits <- hit{m, e}
				return
			}
		}(qType)
	}

	go func() {
		wg.Wait()
		close(hits)
	}()

	for h := range hits {
		if h.found || h.err != nil {
			return h.found, h.err
		}
	}

	return false, nil
}

// MatchMX is similar to MatchIP but first performs an MX lookup on the
// name.  Then it performs an address lookup on each MX name returned.
// Then IPMatcherFunc used to compare checked IP to the returned address(es).
// If any address matches, the mechanism matches
func (r *MiekgDNSResolver) MatchMX(name string, matcher IPMatcherFunc) (bool, error) {
	req := new(dns.Msg)
	req.SetQuestion(name, dns.TypeMX)

	res, err := r.exchange(req)
	if err != nil {
		return false, err
	}

	var wg sync.WaitGroup
	hits := make(chan hit)

	for _, rr := range res.Answer {
		mx, ok := rr.(*dns.MX)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(name string) {
			found, err := r.MatchIP(name, matcher)
			hits <- hit{found, err}
			wg.Done()
		}(mx.Mx)
	}

	go func() {
		wg.Wait()
		close(hits)
	}()

	for h := range hits {
		if h.found || h.err != nil {
			return h.found, h.err
		}
	}

	return false, nil
}
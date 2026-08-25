// Command loadgen fires a file of query names at a DNS server and reports
// throughput, latency and the rcode breakdown.
//
// It exists because `dig -f` is sequential and takes minutes for a few thousand
// queries; the blocklists this repo targets have over half a million domains.
// Driven by scripts/loadtest.sh.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

func main() {
	var (
		server  = flag.String("server", "127.0.0.1:1053", "DNS server to query")
		file    = flag.String("file", "", "file of query names, one per line")
		workers = flag.Int("workers", 32, "concurrent workers")
		expect  = flag.String("expect", "", "rcode every answer must have (e.g. NXDOMAIN)")
		limit   = flag.Int("limit", 0, "stop after this many queries (0 = all)")
		timeout = flag.Duration("timeout", 2*time.Second, "per-query timeout")
	)
	flag.Parse()

	if *file == "" {
		fmt.Fprintln(os.Stderr, "-file is required")
		os.Exit(2)
	}

	names, err := readNames(*file, *limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "no query names")
		os.Exit(1)
	}

	wantRcode := -1
	if *expect != "" {
		code, ok := dns.StringToRcode[strings.ToUpper(*expect)]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown rcode %q\n", *expect)
			os.Exit(2)
		}
		wantRcode = code
	}

	r := run(names, *server, *workers, *timeout, wantRcode)
	r.report(len(names), wantRcode)

	if wantRcode >= 0 && r.mismatches > 0 {
		os.Exit(1)
	}
	if r.errors > 0 {
		os.Exit(1)
	}
}

type results struct {
	mu         sync.Mutex
	latencies  []time.Duration
	rcodes     map[int]int
	errors     int
	mismatches int
	samples    []string // first few errors, for diagnosis
	unexpected []string // first few answers with the wrong rcode
	elapsed    time.Duration
}

func run(names []string, server string, workers int, timeout time.Duration, wantRcode int) *results {
	r := &results{rcodes: map[int]int{}}

	queue := make(chan string, workers*4)
	var wg sync.WaitGroup

	started := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker(queue, server, timeout, wantRcode, r)
		}()
	}

	for _, n := range names {
		queue <- n
	}
	close(queue)
	wg.Wait()

	r.elapsed = time.Since(started)
	return r
}

// worker holds one connection for its lifetime; dialing per query would measure
// the kernel more than the server.
func worker(queue <-chan string, server string, timeout time.Duration, wantRcode int, r *results) {
	client := &dns.Client{Timeout: timeout}
	conn, err := client.Dial(server)
	if err != nil {
		r.mu.Lock()
		r.errors++
		r.mu.Unlock()
		return
	}
	defer conn.Close()

	var (
		latencies  []time.Duration
		rcodes     = map[int]int{}
		errs       int
		samples    []string
		unexpected []string
	)

	for name := range queue {
		msg := new(dns.Msg).SetQuestion(dns.Fqdn(name), dns.TypeA)

		start := time.Now()
		conn.SetDeadline(time.Now().Add(timeout))
		werr := conn.WriteMsg(msg)
		var resp *dns.Msg
		if werr == nil {
			resp, err = conn.ReadMsg()
		} else {
			err = werr
		}
		took := time.Since(start)

		if err != nil || resp == nil {
			errs++
			if len(samples) < 5 {
				samples = append(samples, fmt.Sprintf("%s: %v", name, err))
			}
			// A dead connection stays dead; reconnect rather than burning
			// through the rest of the queue with errors.
			conn.Close()
			if c, derr := client.Dial(server); derr == nil {
				conn = c
			} else {
				break
			}
			continue
		}

		latencies = append(latencies, took)
		rcodes[resp.Rcode]++

		if wantRcode >= 0 && resp.Rcode != wantRcode && len(unexpected) < 10 {
			unexpected = append(unexpected,
				fmt.Sprintf("%s -> %s", name, dns.RcodeToString[resp.Rcode]))
		}
	}

	r.mu.Lock()
	r.latencies = append(r.latencies, latencies...)
	for code, n := range rcodes {
		r.rcodes[code] += n
	}
	r.errors += errs
	r.samples = append(r.samples, samples...)
	r.unexpected = append(r.unexpected, unexpected...)
	r.mu.Unlock()
}

func (r *results) report(total, wantRcode int) {
	answered := len(r.latencies)
	secs := r.elapsed.Seconds()

	fmt.Printf("  queries sent     %d\n", total)
	fmt.Printf("  answered         %d\n", answered)
	fmt.Printf("  errors/timeouts  %d\n", r.errors)
	fmt.Printf("  elapsed          %.2fs\n", secs)
	if secs > 0 {
		fmt.Printf("  throughput       %.0f q/s\n", float64(answered)/secs)
	}

	if answered > 0 {
		sort.Slice(r.latencies, func(i, j int) bool { return r.latencies[i] < r.latencies[j] })
		fmt.Printf("  latency          p50 %s  p90 %s  p99 %s  max %s\n",
			round(pct(r.latencies, 0.50)), round(pct(r.latencies, 0.90)),
			round(pct(r.latencies, 0.99)), round(r.latencies[len(r.latencies)-1]))
	}

	fmt.Printf("  rcodes           ")
	codes := make([]int, 0, len(r.rcodes))
	for c := range r.rcodes {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	parts := make([]string, 0, len(codes))
	for _, c := range codes {
		parts = append(parts, fmt.Sprintf("%s=%d", dns.RcodeToString[c], r.rcodes[c]))
	}
	fmt.Println(strings.Join(parts, "  "))

	if wantRcode >= 0 {
		r.mismatches = answered - r.rcodes[wantRcode]
		if r.mismatches > 0 {
			fmt.Printf("  MISMATCH         %d answers were not %s\n",
				r.mismatches, dns.RcodeToString[wantRcode])
			for i, u := range r.unexpected {
				if i >= 10 {
					break
				}
				fmt.Printf("                   %s\n", u)
			}
		}
	}
	for _, s := range r.samples {
		fmt.Printf("  sample error     %s\n", s)
	}
}

func pct(sorted []time.Duration, p float64) time.Duration {
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

func round(d time.Duration) time.Duration {
	if d < time.Millisecond {
		return d.Round(time.Microsecond)
	}
	return d.Round(10 * time.Microsecond)
}

func readNames(path string, limit int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var names []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		name := strings.TrimSpace(sc.Text())
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		names = append(names, name)
		if limit > 0 && len(names) >= limit {
			break
		}
	}
	return names, sc.Err()
}

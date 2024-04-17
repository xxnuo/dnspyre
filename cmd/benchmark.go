package cmd

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
	"github.com/fatih/color"
	"github.com/miekg/dns"
	"github.com/quic-go/quic-go/http3"
	"github.com/schollz/progressbar/v3"
	"github.com/tantalor93/doh-go/doh"
	"github.com/tantalor93/doq-go/doq"
	"go.uber.org/ratelimit"
	"golang.org/x/net/http2"
)

var client = http.Client{
	Timeout: 120 * time.Second,
}

// defaultEdns0BufferSize default EDNS0 buffer size according to the http://www.dnsflagday.net/2020/
const defaultEdns0BufferSize = 1232

const (
	udpNetwork    = "udp"
	tcpNetwork    = "tcp"
	tcptlsNetwork = "tcp-tls"
	quicNetwork   = "quic"

	getMethod  = "get"
	postMethod = "post"

	http1Proto = "1.1"
	http2Proto = "2"
	http3Proto = "3"
)

// Benchmark is representation of runnable DNS benchmark scenario.
// based on domains provided in Benchmark.Queries, it will be firing DNS queries until
// the desired number of queries have been sent by each concurrent worker (see Benchmark.Count) or the desired
// benchmark duration have been reached (see Benchmark.Duration).
//
// Benchmark will create Benchmark.Concurrency worker goroutines, where each goroutine will be generating DNS queries
// with domains defined using Benchmark.Queries and DNS query types defined in Benchmark.Types. Each worker will
// either generate Benchmark.Types*Benchmark.Count*len(Benchmark.Queries) number of queries if Benchmark.Count is specified,
// or the worker will be generating arbitrary number of queries until Benchmark.Duration is reached.
type Benchmark struct {
	// Server represents (plain DNS, DoT, DoH or DoQ) server, which will be benchmarked.
	// Format depends on the DNS protocol, that should be used for DNS benchmark.
	// For plain DNS (either over UDP or TCP) the format is <IP/host>[:port], if port is not provided then port 53 is used.
	// For DoT the format is <IP/host>[:port], if port is not provided then port 853 is used.
	// For DoH the format is https://<IP/host>[:port][/path] or http://<IP/host>[:port][/path], if port is not provided then either 443 or 80 port is used. If no path is provided, then /dns-query is used.
	// For DoQ the format is quic://<IP/host>[:port], if port is not provided then port 853 is used.
	Server string

	// Types is an array of DNS query types, that should be used in benchmark. All domains retrieved from domain data source will be fired with each
	// type specified here.
	Types []string

	// Count specifies how many times each domain from data source is used by each worker. Either Benchmark.Count or Benchmark.Duration must be specified.
	// If Benchmark.Count and Benchmark.Duration is specified at once, it is considered invalid state of Benchmark.
	Count int64

	// Duration specifies for how long the benchmark should be executing, the benchmark will run for the specified time
	// while sending DNS requests in an infinite loop based on the data source. After running for the specified duration, the benchmark is canceled.
	// This option is exclusive with Benchmark.Count.
	Duration time.Duration

	// Concurrency controls how many concurrent queries will be issued at once. Benchmark will spawn Concurrency number of parallel worker goroutines.
	Concurrency uint32

	// Rate configures global rate limit for queries per second. This limit is shared between all the worker goroutines. This means that queries generated by this Benchmark
	// per second will not exceed this limit.
	Rate int
	// RateLimitWorker configures rate limit per worker for queries per second. This means that queries generated by each concurrent worker per second will not exceed this limit.
	RateLimitWorker int

	// QperConn configures how many queries are sent by each connection (socket) before closing it and creating a new one.
	// This is considered only for plain DNS over UDP or TCP and DoT.
	QperConn int64

	// Recurse configures whether the DNS queries generated by this Benchmark have Recursion Desired (RD) flag set.
	Recurse bool

	// Probability is used to bring randomization into Benchmark runs. When Probability is 1 or above, then all the domains passed in Queries field will be used during Benchmark run.
	// When Probability is less than 1 and more than 0, then each domain in Queries has Probability chance to be used during benchmark.
	// When Probability is less than 0, then no domain from Queries is used during benchmark.
	Probability float64

	// EdnsOpt specifies EDNS option with code point code and optionally payload of value as a hexadecimal string in format code[:value].
	// code must be an arbitrary numeric value.
	EdnsOpt string

	// DNSSEC Allow DNSSEC (sets DO bit for all DNS requests to 1)
	DNSSEC bool

	// Edns0 configures EDNS0 usage in DNS requests send by benchmark and configures EDNS0 buffer size to the specified value. When 0 is configured, then EDNS0 is not used.
	Edns0 uint16

	// TCP controls whether plain DNS benchmark uses TCP or UDP. When true, the TCP is used.
	TCP bool

	// DOT controls whether DoT is used for the benchmark.
	DOT bool

	// WriteTimeout configures write timeout for DNS requests generated by Benchmark.
	WriteTimeout time.Duration
	// ReadTimeout configures read timeout for DNS responses.
	ReadTimeout time.Duration
	// ConnectTimeout configures timeout for connection establishment.
	ConnectTimeout time.Duration
	// RequestTimeout configures overall timeout for a single DNS request.
	RequestTimeout time.Duration

	// Rcodes controls whether ResultStats.Codes is filled in Benchmark results.
	Rcodes bool

	// HistDisplay controls whether Benchmark.PrintReport will include histogram.
	HistDisplay bool
	// HistMin controls minimum value of histogram printed by Benchmark.PrintReport.
	HistMin time.Duration
	// HistMax controls maximum value of histogram printed by Benchmark.PrintReport.
	HistMax time.Duration
	// HistPre controls precision of histogram printed by Benchmark.PrintReport.
	HistPre int

	// Csv path to file, where the Benchmark result distribution is written.
	Csv string
	// JSON controls whether the Benchmark.PrintReport prints the Benchmark results in JSON format (option is true).
	JSON bool

	// Silent controls whether the Benchmark.Run and Benchmark.PrintReport writes anything to stdout.
	Silent bool
	// Color controls coloring of std output.
	Color bool

	// PlotDir controls where the generated graphs are exported. If set to empty (""), which is default value. Then no graphs are generated.
	PlotDir string
	// PlotFormat controls the format of generated graphs. Supported values are "svg", "png" and "jpg".
	PlotFormat string

	// DohMethod controls HTTP method used for sending DoH requests. Supported values are "post" and "get". Default is "post".
	DohMethod string
	// DohProtocol controls HTTP protocol version used fo sending DoH requests. Supported values are "1.1", "2" and "3". Default is "1.1".
	DohProtocol string

	// Insecure disables server TLS certificate validation. Applicable for DoT, DoH and DoQ.
	Insecure bool

	// ProgressBar controls whether the progress bar is printed.
	ProgressBar bool

	// Queries list of domains and data sources to be used in Benchmark. It can contain a local file data source referenced using @<file-path>, for example @data/2-domains.
	// It can also be data source file accessible using HTTP, like https://raw.githubusercontent.com/Tantalor93/dnspyre/master/data/1000-domains, in that case the file will be downloaded and saved in-memory.
	// These data sources can be combined, for example "google.com @data/2-domains https://raw.githubusercontent.com/Tantalor93/dnspyre/master/data/2-domains".
	Queries []string

	// internal variable so we do not have to parse the address with each request.
	useDoH  bool
	useQuic bool
}

type queryFunc func(context.Context, string, *dns.Msg) (*dns.Msg, error)

// prepare validates and normalizes Benchmark settings.
func (b *Benchmark) prepare() error {
	if len(b.Server) == 0 {
		return errors.New("server for benchmarking must not be empty")
	}

	b.useDoH, _ = isHTTPUrl(b.Server)
	b.useQuic = strings.HasPrefix(b.Server, "quic://")
	if b.useQuic {
		b.Server = strings.TrimPrefix(b.Server, "quic://")
	}

	if b.useDoH {
		parsedURL, err := url.Parse(b.Server)
		if err != nil {
			return err
		}
		if len(parsedURL.Path) == 0 {
			b.Server += "/dns-query"
		}
	}

	b.addPortIfMissing()

	if b.Count == 0 && b.Duration == 0 {
		b.Count = 1
	}

	if b.Duration > 0 && b.Count > 0 {
		return errors.New("--number and --duration is specified at once, only one can be used")
	}

	if b.HistMax == 0 {
		b.HistMax = b.RequestTimeout
	}

	if b.Edns0 != 0 && (b.Edns0 < 512 || b.Edns0 > 4096) {
		return errors.New("--edns0 must have value between 512 and 4096")
	}

	if len(b.EdnsOpt) != 0 {
		split := strings.Split(b.EdnsOpt, ":")
		if len(split) != 2 {
			return errors.New("--ednsopt is not in correct format")
		}
		_, err := hex.DecodeString(split[1])
		if err != nil {
			return errors.New("--ednsopt is not in correct format, data is not hexadecimal string")
		}
		_, err = strconv.ParseUint(split[0], 10, 16)
		if err != nil {
			return errors.New("--ednsopt is not in correct format, code is not a decimal number")
		}
	}

	return nil
}

// Run executes benchmark, if benchmark is unable to start the error is returned, otherwise array of results from parallel benchmark goroutines is returned.
func (b *Benchmark) Run(ctx context.Context) ([]*ResultStats, error) {
	color.NoColor = !b.Color

	if err := b.prepare(); err != nil {
		return nil, err
	}

	questions, err := b.prepareQuestions()
	if err != nil {
		return nil, err
	}

	if b.Duration != 0 {
		timeoutCtx, cancel := context.WithTimeout(ctx, b.Duration)
		ctx = timeoutCtx
		defer cancel()
	}

	if !b.Silent && !b.JSON {
		fmt.Printf("Using %s hostnames\n", highlightStr(len(questions)))
	}

	var qTypes []uint16
	for _, v := range b.Types {
		qTypes = append(qTypes, dns.StringToType[v])
	}

	network := udpNetwork
	if b.TCP {
		network = tcpNetwork
	}
	if b.DOT {
		network = tcptlsNetwork
	}

	var query queryFunc
	if b.useDoH {
		var dohQuery queryFunc
		dohQuery, network = b.getDoHClient()
		query = func(ctx context.Context, s string, msg *dns.Msg) (*dns.Msg, error) {
			return dohQuery(ctx, s, msg)
		}
	}

	if b.useQuic {
		h, _, _ := net.SplitHostPort(b.Server)
		// nolint:gosec
		quicClient := doq.NewClient(b.Server, doq.Options{
			TLSConfig:      &tls.Config{ServerName: h, InsecureSkipVerify: b.Insecure},
			ReadTimeout:    b.ReadTimeout,
			WriteTimeout:   b.WriteTimeout,
			ConnectTimeout: b.ConnectTimeout,
		})
		query = func(ctx context.Context, _ string, msg *dns.Msg) (*dns.Msg, error) {
			return quicClient.Send(ctx, msg)
		}
		network = quicNetwork
	}

	limits := ""
	var limit ratelimit.Limiter
	if b.Rate > 0 {
		limit = ratelimit.New(b.Rate)
		if b.RateLimitWorker == 0 {
			limits = fmt.Sprintf("(limited to %s QPS overall)", highlightStr(b.Rate))
		} else {
			limits = fmt.Sprintf("(limited to %s QPS overall and %s QPS per concurrent worker)", highlightStr(b.Rate), highlightStr(b.RateLimitWorker))
		}
	}
	if b.Rate == 0 && b.RateLimitWorker > 0 {
		limits = fmt.Sprintf("(limited to %s QPS per concurrent worker)", highlightStr(b.RateLimitWorker))
	}

	if !b.Silent && !b.JSON {
		fmt.Printf("Benchmarking %s via %s with %s concurrent requests %s\n", highlightStr(b.Server), highlightStr(network), highlightStr(b.Concurrency), limits)
	}

	var bar *progressbar.ProgressBar
	var incrementBar bool
	if repetitions := b.Count * int64(b.Concurrency) * int64(len(b.Types)) * int64(len(questions)); !b.Silent && b.ProgressBar && repetitions >= 100 {
		fmt.Println()
		if b.Probability < 1.0 {
			// show spinner when Benchmark.Probability is less than 1.0, because the actual number of repetitions is not known
			repetitions = -1
		}
		bar = progressbar.Default(repetitions, "Progress:")
		incrementBar = true
	}
	if !b.Silent && b.ProgressBar && b.Duration >= 10*time.Second {
		fmt.Println()
		bar = progressbar.Default(int64(b.Duration.Seconds()), "Progress:")
		ticker := time.NewTicker(time.Second)
		go func() {
			for {
				select {
				case <-ticker.C:
					bar.Add(1)
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	stats := make([]*ResultStats, b.Concurrency)

	var wg sync.WaitGroup
	var w uint32
	for w = 0; w < b.Concurrency; w++ {
		st := &ResultStats{Hist: hdrhistogram.New(b.HistMin.Nanoseconds(), b.HistMax.Nanoseconds(), b.HistPre)}
		stats[w] = st
		if b.Rcodes {
			st.Codes = make(map[int]int64)
		}
		st.Qtypes = make(map[string]int64)
		if b.useDoH {
			st.DoHStatusCodes = make(map[int]int64)
		}
		st.Counters = &Counters{}

		var err error
		wg.Add(1)
		go func(st *ResultStats) {
			defer func() {
				wg.Done()
			}()

			// create a new lock free rand source for this goroutine
			// nolint:gosec
			rando := rand.New(rand.NewSource(time.Now().UnixNano()))

			var workerLimit ratelimit.Limiter
			if b.RateLimitWorker > 0 {
				workerLimit = ratelimit.New(b.RateLimitWorker)
			}

			var i int64

			// shadow & copy the query func, because for DoQ and DoH we want to share the client, for plain DNS and DoT we don't
			// due to manual connection redialing on error, etc.
			query := query

			if query == nil {
				dnsClient := b.getDNSClient()

				var co *dns.Conn
				query = func(ctx context.Context, s string, msg *dns.Msg) (*dns.Msg, error) {
					if co != nil && b.QperConn > 0 && i%b.QperConn == 0 {
						co.Close()
						co = nil
					}

					if co == nil {
						co, err = dnsClient.DialContext(ctx, b.Server)
						if err != nil {
							return nil, err
						}
					}
					r, _, err := dnsClient.ExchangeWithConnContext(ctx, msg, co)
					if err != nil {
						co.Close()
						co = nil
						return nil, err
					}
					return r, nil
				}
			}

			for i = 0; i < b.Count || b.Duration != 0; i++ {
				for _, q := range questions {
					for _, qt := range qTypes {
						if ctx.Err() != nil {
							return
						}
						if rando.Float64() > b.Probability {
							continue
						}
						if limit != nil {
							if err := checkLimit(ctx, limit); err != nil {
								return
							}
						}
						if workerLimit != nil {
							if err := checkLimit(ctx, workerLimit); err != nil {
								return
							}
						}
						var resp *dns.Msg

						m := dns.Msg{}
						m.RecursionDesired = b.Recurse

						m.Question = make([]dns.Question, 1)
						question := dns.Question{Name: q, Qtype: qt, Qclass: dns.ClassINET}
						m.Question[0] = question

						if b.useQuic {
							m.Id = 0
						} else {
							m.Id = uint16(rando.Uint32())
						}

						if b.Edns0 > 0 {
							m.SetEdns0(b.Edns0, false)
						}
						if ednsOpt := b.EdnsOpt; len(ednsOpt) > 0 {
							addEdnsOpt(&m, ednsOpt)
						}
						if b.DNSSEC {
							edns0 := m.IsEdns0()
							if edns0 == nil {
								m.SetEdns0(defaultEdns0BufferSize, false)
								edns0 = m.IsEdns0()
							}
							edns0.SetDo(true)
						}

						start := time.Now()

						reqTimeoutCtx, cancel := context.WithTimeout(ctx, b.RequestTimeout)
						resp, err = query(reqTimeoutCtx, b.Server, &m)
						cancel()
						st.record(&m, resp, err, start, time.Since(start))

						if incrementBar {
							bar.Add(1)
						}
					}
				}
			}
		}(st)
	}

	wg.Wait()

	return stats, nil
}

func addEdnsOpt(m *dns.Msg, ednsOpt string) {
	o := m.IsEdns0()
	if o == nil {
		m.SetEdns0(defaultEdns0BufferSize, false)
		o = m.IsEdns0()
	}
	s := strings.Split(ednsOpt, ":")
	data, _ := hex.DecodeString(s[1])
	code, _ := strconv.ParseUint(s[0], 10, 16)
	o.Option = append(o.Option, &dns.EDNS0_LOCAL{Code: uint16(code), Data: data})
}

func (b *Benchmark) addPortIfMissing() {
	if b.useDoH {
		// both HTTPS and HTTP are using default ports 443 and 80 if no other port is specified
		return
	}
	if _, _, err := net.SplitHostPort(b.Server); err != nil {
		if b.DOT {
			// https://www.rfc-editor.org/rfc/rfc7858
			b.Server = net.JoinHostPort(b.Server, "853")
			return
		}
		if b.useQuic {
			// https://datatracker.ietf.org/doc/rfc9250
			b.Server = net.JoinHostPort(b.Server, "853")
			return
		}
		b.Server = net.JoinHostPort(b.Server, "53")
		return
	}
}

func isHTTPUrl(s string) (ok bool, network string) {
	if strings.HasPrefix(s, "http://") {
		return true, "http"
	}
	if strings.HasPrefix(s, "https://") {
		return true, "https"
	}
	return false, ""
}

func (b *Benchmark) getDoHClient() (queryFunc, string) {
	_, network := isHTTPUrl(b.Server)
	var tr http.RoundTripper
	network += "/"
	switch b.DohProtocol {
	case http3Proto:
		network += http3Proto
		// nolint:gosec
		tr = &http3.RoundTripper{TLSClientConfig: &tls.Config{InsecureSkipVerify: b.Insecure}}
	case http2Proto:
		network += http2Proto
		// nolint:gosec
		tr = &http2.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: b.Insecure}}
	case http1Proto:
		fallthrough
	default:
		network += http1Proto
		// nolint:gosec
		tr = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: b.Insecure}}
	}
	c := http.Client{Transport: tr, Timeout: b.ReadTimeout}
	dohClient := doh.NewClient(&c)

	switch b.DohMethod {
	case postMethod:
		network += " (POST)"
		return dohClient.SendViaPost, network
	case getMethod:
		network += " (GET)"
		return dohClient.SendViaGet, network
	default:
		network += " (POST)"
		return dohClient.SendViaPost, network
	}
}

func (b *Benchmark) getDNSClient() *dns.Client {
	network := udpNetwork
	if b.TCP {
		network = tcpNetwork
	}
	if b.DOT {
		network = tcptlsNetwork
	}

	return &dns.Client{
		Net:          network,
		DialTimeout:  b.ConnectTimeout,
		WriteTimeout: b.WriteTimeout,
		ReadTimeout:  b.ReadTimeout,
		Timeout:      b.RequestTimeout,
		// nolint:gosec
		TLSConfig: &tls.Config{InsecureSkipVerify: b.Insecure},
	}
}

func (b *Benchmark) prepareQuestions() ([]string, error) {
	var questions []string
	for _, q := range b.Queries {
		if ok, _ := isHTTPUrl(q); ok {
			resp, err := client.Get(q)
			if err != nil {
				return nil, fmt.Errorf("failed to download file '%s' with error '%v'", q, err)
			}
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				return nil, fmt.Errorf("failed to download file '%s' with status '%s'", q, resp.Status)
			}
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				questions = append(questions, dns.Fqdn(scanner.Text()))
			}
		} else {
			questions = append(questions, dns.Fqdn(q))
		}
	}
	return questions, nil
}

func checkLimit(ctx context.Context, limiter ratelimit.Limiter) error {
	done := make(chan struct{})
	go func() {
		limiter.Take()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

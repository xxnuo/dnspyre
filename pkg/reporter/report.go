package reporter

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
	"github.com/tantalor93/dnspyre/v3/pkg/dnsbench"
)

type orderedMap struct {
	m     map[string]int
	order []string
}

type reportParameters struct {
	benchmark                 *dnsbench.Benchmark
	outputWriter              io.Writer
	timings                   *hdrhistogram.Histogram
	codeTotals                map[int]int64
	totalCounters             dnsbench.Counters
	qtypeTotals               map[string]int64
	topErrs                   orderedMap
	authenticatedDomains      map[string]struct{}
	benchmarkDuration         time.Duration
	dohResponseStatusesTotals map[int]int64
}

// PrintReport prints formatted benchmark result to stdout, exports graphs and generates CSV output if configured.
// If there is a fatal error while printing report, an error is returned.
func PrintReport(b *dnsbench.Benchmark, stats []*dnsbench.ResultStats, benchStart time.Time, benchDuration time.Duration) error {
	// merge all the stats here
	timings := hdrhistogram.New(b.HistMin.Nanoseconds(), b.HistMax.Nanoseconds(), b.HistPre)
	codeTotals := make(map[int]int64)
	qtypeTotals := make(map[string]int64)
	dohResponseStatusesTotals := make(map[int]int64)

	times := make([]dnsbench.Datapoint, 0)
	errTimes := make([]dnsbench.ErrorDatapoint, 0)

	errs := make(map[string]int, 0)
	top3errs := make(map[string]int)
	top3errorsInOrder := make([]string, 0)

	var totalCounters dnsbench.Counters

	// TODO proper coverage of plots
	for _, s := range stats {
		for _, err := range s.Errors {
			errorString := errString(err)

			if v, ok := errs[errorString]; ok {
				errs[errorString] = v + 1
			} else {
				errs[errorString] = 1
			}
		}
		errTimes = append(errTimes, s.Errors...)

		timings.Merge(s.Hist)
		times = append(times, s.Timings...)
		if s.Codes != nil {
			for k, v := range s.Codes {
				codeTotals[k] += v
			}
		}
		if s.Qtypes != nil {
			for k, v := range s.Qtypes {
				qtypeTotals[k] += v
			}
		}
		if s.DoHStatusCodes != nil {
			for k, v := range s.DoHStatusCodes {
				dohResponseStatusesTotals[k] += v
			}
		}
		if s.Counters != nil {
			totalCounters = dnsbench.Counters{
				Total:      totalCounters.Total + s.Counters.Total,
				IOError:    totalCounters.IOError + s.Counters.IOError,
				Success:    totalCounters.Success + s.Counters.Success,
				Negative:   totalCounters.Negative + s.Counters.Negative,
				Error:      totalCounters.Error + s.Counters.Error,
				IDmismatch: totalCounters.IDmismatch + s.Counters.IDmismatch,
				Truncated:  totalCounters.Truncated + s.Counters.Truncated,
			}
		}
	}

	for i := 0; i < 3; i++ {
		max := 0
		maxerr := ""
		for k, v := range errs {
			if _, ok := top3errs[k]; v > max && !ok {
				maxerr = k
				max = v
			}
		}
		if max != 0 {
			top3errs[maxerr] = max
			top3errorsInOrder = append(top3errorsInOrder, maxerr)
		}
	}

	// sort data points from the oldest to the earliest, so we can better plot time dependant graphs (like line)
	sort.SliceStable(times, func(i, j int) bool {
		return times[i].Start.Before(times[j].Start)
	})

	// sort error data points from the oldest to the earliest, so we can better plot time dependant graphs (like line)
	sort.SliceStable(errTimes, func(i, j int) bool {
		return errTimes[i].Start.Before(errTimes[j].Start)
	})

	if len(b.PlotDir) != 0 {
		now := time.Now().Format(time.RFC3339)
		dir := fmt.Sprintf("%s/graphs-%s", b.PlotDir, now)
		if err := os.Mkdir(dir, os.ModePerm); err != nil {
			panic(err)
		}
		plotHistogramLatency(fileName(b, dir, "latency-histogram"), times)
		plotBoxPlotLatency(fileName(b, dir, "latency-boxplot"), b.Server, times)
		plotResponses(fileName(b, dir, "responses-barchart"), codeTotals)
		plotLineThroughput(fileName(b, dir, "throughput-lineplot"), benchStart, times)
		plotLineLatencies(fileName(b, dir, "latency-lineplot"), benchStart, times)
		plotErrorRate(fileName(b, dir, "errorrate-lineplot"), benchStart, errTimes)
	}

	var csv *os.File
	if b.Csv != "" {
		f, err := os.Create(b.Csv)
		if err != nil {
			return fmt.Errorf("failed to create file for CSV export due to '%v'", err)
		}

		csv = f
	}

	defer func() {
		if csv != nil {
			csv.Close()
		}
	}()

	if csv != nil {
		writeBars(csv, timings.Distribution())
	}

	authenticatedDomains := make(map[string]struct{})
	if b.DNSSEC {
		for _, v := range stats {
			for k := range v.AuthenticatedDomains {
				authenticatedDomains[k] = struct{}{}
			}
		}
	}

	if b.Silent {
		return nil
	}
	topErrs := orderedMap{m: top3errs, order: top3errorsInOrder}
	params := reportParameters{
		benchmark:                 b,
		outputWriter:              b.Writer,
		timings:                   timings,
		codeTotals:                codeTotals,
		totalCounters:             totalCounters,
		qtypeTotals:               qtypeTotals,
		topErrs:                   topErrs,
		authenticatedDomains:      authenticatedDomains,
		benchmarkDuration:         benchDuration,
		dohResponseStatusesTotals: dohResponseStatusesTotals,
	}
	if b.JSON {
		j := jsonReporter{}
		return j.print(params)
	}
	s := standardReporter{}
	return s.print(params)
}

func errString(err dnsbench.ErrorDatapoint) string {
	var errorString string
	var netOpErr *net.OpError
	var resolveErr *net.DNSError

	switch {
	case errors.As(err.Err, &resolveErr):
		errorString = resolveErr.Err + " " + resolveErr.Name
	case errors.As(err.Err, &netOpErr):
		errorString = netOpErr.Op + " " + netOpErr.Net
		if netOpErr.Addr != nil {
			errorString += " " + netOpErr.Addr.String()
		}
	default:
		errorString = err.Err.Error()
	}
	return errorString
}

func fileName(b *dnsbench.Benchmark, dir, name string) string {
	return dir + "/" + name + "." + b.PlotFormat
}

func writeBars(f *os.File, bars []hdrhistogram.Bar) {
	f.WriteString("From (ns), To (ns), Count\n")

	for _, b := range bars {
		f.WriteString(b.String())
	}
}
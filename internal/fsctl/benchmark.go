package fsctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"time"
)

type BenchmarkOptions struct {
	SequentialBlockBytes int
	RandomBlockBytes     int
	RandomReads          int
	Seed                 int64
	BypassOSCache        bool
}

type SequentialMetric struct {
	Bytes          int64         `json:"bytes"`
	Duration       time.Duration `json:"duration"`
	BytesPerSecond float64       `json:"bytes_per_second"`
}

type RandomMetric struct {
	Reads int           `json:"reads"`
	P50   time.Duration `json:"p50"`
	P95   time.Duration `json:"p95"`
	P99   time.Duration `json:"p99"`
}

type BenchmarkReport struct {
	Native                 SequentialMetric `json:"native"`
	Virtual                SequentialMetric `json:"virtual"`
	Random                 RandomMetric     `json:"random"`
	GoSysBytes             uint64           `json:"go_sys_bytes"`
	OSCacheBypassRequested bool             `json:"os_cache_bypass_requested"`
	OSCacheBypassApplied   bool             `json:"os_cache_bypass_applied"`
}

func Benchmark(ctx context.Context, nativePath string, virtual Readable, options BenchmarkOptions) (BenchmarkReport, error) {
	if options.SequentialBlockBytes <= 0 {
		options.SequentialBlockBytes = 1 << 20
	}
	if options.RandomBlockBytes <= 0 {
		options.RandomBlockBytes = 4 << 10
	}
	if options.RandomReads <= 0 {
		options.RandomReads = 1000
	}
	native, err := os.Open(nativePath)
	if err != nil {
		return BenchmarkReport{}, err
	}
	defer native.Close()
	bypassApplied := false
	if options.BypassOSCache {
		bypassApplied, err = configureNoCache(native)
		if err != nil {
			return BenchmarkReport{}, err
		}
	}
	info, err := native.Stat()
	if err != nil {
		return BenchmarkReport{}, err
	}
	if info.Size() != virtual.Size() {
		return BenchmarkReport{}, errors.New("benchmark native and virtual sizes differ")
	}
	nativeMetric, err := benchmarkNativeSequential(ctx, native, info.Size(), options.SequentialBlockBytes)
	if err != nil {
		return BenchmarkReport{}, err
	}
	virtualMetric, err := benchmarkVirtualSequential(ctx, virtual, options.SequentialBlockBytes)
	if err != nil {
		return BenchmarkReport{}, err
	}
	randomMetric, err := benchmarkRandom(ctx, native, virtual, info.Size(), options)
	if err != nil {
		return BenchmarkReport{}, err
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return BenchmarkReport{Native: nativeMetric, Virtual: virtualMetric, Random: randomMetric, GoSysBytes: memory.Sys, OSCacheBypassRequested: options.BypassOSCache, OSCacheBypassApplied: bypassApplied}, nil
}

func benchmarkNativeSequential(ctx context.Context, file *os.File, size int64, blockBytes int) (SequentialMetric, error) {
	buffer := make([]byte, blockBytes)
	start := time.Now()
	var offset int64
	for offset < size {
		if err := ctx.Err(); err != nil {
			return SequentialMetric{}, err
		}
		need := blockBytes
		if remaining := size - offset; int64(need) > remaining {
			need = int(remaining)
		}
		n, err := file.ReadAt(buffer[:need], offset)
		if n != need || (err != nil && !errors.Is(err, io.EOF)) {
			return SequentialMetric{}, fmt.Errorf("native sequential read at %d: n=%d err=%v", offset, n, err)
		}
		offset += int64(n)
	}
	return sequentialMetric(offset, time.Since(start)), nil
}

func benchmarkVirtualSequential(ctx context.Context, virtual Readable, blockBytes int) (SequentialMetric, error) {
	buffer := make([]byte, blockBytes)
	start := time.Now()
	var offset int64
	for offset < virtual.Size() {
		need := blockBytes
		if remaining := virtual.Size() - offset; int64(need) > remaining {
			need = int(remaining)
		}
		n, err := virtual.ReadAt(ctx, buffer[:need], offset)
		if n != need || (err != nil && !errors.Is(err, io.EOF)) {
			return SequentialMetric{}, fmt.Errorf("virtual sequential read at %d: n=%d err=%v", offset, n, err)
		}
		offset += int64(n)
	}
	return sequentialMetric(offset, time.Since(start)), nil
}

func benchmarkRandom(ctx context.Context, native *os.File, virtual Readable, size int64, options BenchmarkOptions) (RandomMetric, error) {
	if size == 0 {
		return RandomMetric{}, nil
	}
	random := rand.New(rand.NewSource(options.Seed))
	nativeBuffer := make([]byte, options.RandomBlockBytes)
	virtualBuffer := make([]byte, options.RandomBlockBytes)
	durations := make([]time.Duration, 0, options.RandomReads)
	for index := 0; index < options.RandomReads; index++ {
		offset := random.Int63n(size)
		length := options.RandomBlockBytes
		if remaining := size - offset; int64(length) > remaining {
			length = int(remaining)
		}
		nativeN, nativeErr := native.ReadAt(nativeBuffer[:length], offset)
		start := time.Now()
		virtualN, virtualErr := virtual.ReadAt(ctx, virtualBuffer[:length], offset)
		duration := time.Since(start)
		if duration <= 0 {
			duration = time.Nanosecond
		}
		if nativeN != length || virtualN != length || !bytes.Equal(nativeBuffer[:length], virtualBuffer[:length]) || (nativeErr != nil && !errors.Is(nativeErr, io.EOF)) || (virtualErr != nil && !errors.Is(virtualErr, io.EOF)) {
			return RandomMetric{}, fmt.Errorf("random benchmark read %d differs at offset %d", index, offset)
		}
		durations = append(durations, duration)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return RandomMetric{Reads: len(durations), P50: percentile(durations, 50), P95: percentile(durations, 95), P99: percentile(durations, 99)}, nil
}

func sequentialMetric(bytesRead int64, duration time.Duration) SequentialMetric {
	if duration <= 0 {
		duration = time.Nanosecond
	}
	return SequentialMetric{Bytes: bytesRead, Duration: duration, BytesPerSecond: float64(bytesRead) / duration.Seconds()}
}

func percentile(values []time.Duration, percent int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := (len(values)*percent + 99) / 100
	if index < 1 {
		index = 1
	}
	return values[index-1]
}

// Command diskwrite measures raw disk write throughput: it writes a file of
// arbitrary data in fixed-size blocks, then fsyncs it. It reports write-only
// and write+sync throughput so disk speed can be compared against network
// in isolation. No network, no HF, no Python — just the disk.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	out := flag.String("out", "", "output file path")
	sizeStr := flag.String("size", "10GiB", "total bytes to write (e.g. 10GiB, 512MiB)")
	bsStr := flag.String("bs", "1MiB", "block size (e.g. 1MiB)")
	results := flag.String("results", "", "append a TSV row to this path")
	flag.Parse()

	if *out == "" {
		log.Fatal("usage: diskwrite --out <file> [--size 10GiB] [--bs 1MiB] [--results file]")
	}
	size, err := parseSize(*sizeStr)
	if err != nil {
		log.Fatalf("size: %v", err)
	}
	bs, err := parseSize(*bsStr)
	if err != nil {
		log.Fatalf("bs: %v", err)
	}
	if bs <= 0 || size <= 0 {
		log.Fatal("size and bs must be > 0")
	}

	buf := make([]byte, bs)
	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer os.Remove(*out)

	written := int64(0)
	writeStart := time.Now()
	for written < size {
		n := int64(bs)
		if written+n > size {
			n = size - written
		}
		if _, err := f.Write(buf[:n]); err != nil {
			f.Close()
			log.Fatalf("write at %d: %v", written, err)
		}
		written += n
	}
	writeElapsed := time.Since(writeStart)

	syncStart := time.Now()
	if err := f.Sync(); err != nil {
		f.Close()
		log.Fatalf("sync: %v", err)
	}
	syncElapsed := time.Since(syncStart)

	if err := f.Close(); err != nil {
		log.Fatal(err)
	}

	total := writeElapsed + syncElapsed
	writeMib := mib(written, writeElapsed)
	totalMib := mib(written, total)
	gib := float64(written) / (1 << 30)

	fmt.Printf("diskwrite: %d bytes (%.2f GiB)\n", written, gib)
	fmt.Printf("  write: %.3fs  %.1f MiB/s\n", writeElapsed.Seconds(), writeMib)
	fmt.Printf("  sync:  %.3fs\n", syncElapsed.Seconds())
	fmt.Printf("  total: %.3fs  %.1f MiB/s\n", total.Seconds(), totalMib)

	if *results != "" {
		if err := appendRow(*results, written, writeElapsed, syncElapsed, total); err != nil {
			log.Printf("warning: write results: %v", err)
		}
	}
}

func mib(b int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(b) / d.Seconds() / (1 << 20)
}

func appendRow(path string, bytes int64, write, sync, total time.Duration) error {
	header := false
	if _, err := os.Stat(path); os.IsNotExist(err) {
		header = true
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if header {
		fmt.Fprintln(f, "bytes\tgib\twrite_s\tsync_s\ttotal_s\twrite_mib_s\ttotal_mib_s")
	}
	fmt.Fprintf(f, "%d\t%.2f\t%.3f\t%.3f\t%.3f\t%.1f\t%.1f\n",
		bytes, float64(bytes)/(1<<30),
		write.Seconds(), sync.Seconds(), total.Seconds(),
		mib(bytes, write), mib(bytes, total))
	return nil
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	switch strings.ToLower(strings.TrimSpace(s[i:])) {
	case "", "b":
		return n, nil
	case "kib":
		return n << 10, nil
	case "mib":
		return n << 20, nil
	case "gib":
		return n << 30, nil
	case "tib":
		return n << 40, nil
	case "kb":
		return n * 1000, nil
	case "mb":
		return n * 1000 * 1000, nil
	case "gb":
		return n * 1000 * 1000 * 1000, nil
	default:
		return 0, fmt.Errorf("unknown unit in %q", s)
	}
}

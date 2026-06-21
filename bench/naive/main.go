// Command naive downloads a Hugging Face model using only the Go standard
// library, sequentially, and profiles each file: it separates the time to
// read a file over the network (into memory) from the time to write it to
// disk, so disk and network can be compared in isolation.
//
// It is the "no supply chain" baseline against the official hf CLI
// (huggingface_hub + hf_xet). No Python, no Xet plugin, no concurrency.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type entry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

const hub = "https://huggingface.co"

func main() {
	repo := flag.String("repo", "", "Hugging Face repo id, e.g. Qwen/Qwen2.5-72B-Instruct")
	rev := flag.String("revision", "main", "revision (branch or commit)")
	out := flag.String("out", "", "output directory")
	sync := flag.Bool("sync", false, "fsync each file after writing (measures real disk, not page cache)")
	results := flag.String("results", "", "write per-file TSV results to this path")
	flag.Parse()

	if *repo == "" || *out == "" {
		log.Fatal("usage: naive --repo <org/name> --out <dir> [--revision main] [--sync] [--results file]")
	}
	token := os.Getenv("HF_TOKEN")

	ctx := context.Background()
	files, err := listTree(ctx, *repo, *rev, token)
	if err != nil {
		log.Fatalf("list tree: %v", err)
	}
	log.Printf("listed %d files", len(files))

	var rows []row
	var totalBytes int64
	var totalNet, totalDisk time.Duration
	start := time.Now()

	for i, f := range files {
		n, netT, diskT, err := fetchFile(ctx, *repo, *rev, f.Path, *out, token, *sync)
		if err != nil {
			log.Fatalf("[%d/%d] %s: %v", i+1, len(files), f.Path, err)
		}
		totalBytes += n
		totalNet += netT
		totalDisk += diskT
		log.Printf("[%d/%d] %s: %d bytes net=%.3fs (%.1f MiB/s) disk=%.3fs (%.1f MiB/s)",
			i+1, len(files), f.Path, n, netT.Seconds(), mib(n, netT), diskT.Seconds(), mib(n, diskT))
		rows = append(rows, row{f.Path, n, netT, diskT})
	}

	wall := time.Since(start)
	fmt.Printf("naive: files=%d bytes=%d (%.2f GiB) net=%.3fs disk=%.3fs wall=%.3fs | net=%.1f MiB/s disk=%.1f MiB/s wall=%.1f MiB/s\n",
		len(rows), totalBytes, float64(totalBytes)/(1<<30),
		totalNet.Seconds(), totalDisk.Seconds(), wall.Seconds(),
		mib(totalBytes, totalNet), mib(totalBytes, totalDisk), float64(totalBytes)/wall.Seconds()/(1<<20))

	if *results != "" {
		if err := writeResults(*results, rows, totalBytes, totalNet, totalDisk); err != nil {
			log.Printf("warning: write results: %v", err)
		}
	}
}

type row struct {
	path  string
	bytes int64
	net   time.Duration
	disk  time.Duration
}

func mib(b int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(b) / d.Seconds() / (1 << 20)
}

// fetchFile reads a file fully into memory (network time, isolated from
// disk) then writes it to a .part file and renames (disk time, isolated
// from network). The two phases are sequential by design: this measures the
// components separately rather than overlapping them.
func fetchFile(ctx context.Context, repo, rev, path, out, token string, doSync bool) (n int64, netT, diskT time.Duration, err error) {
	dest := filepath.Join(out, path)
	if err = os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/%s/resolve/%s/%s", hub, repo, rev, path), nil)
	if err != nil {
		return
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	netStart := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		err = fmt.Errorf("%s: %s", path, resp.Status)
		return
	}
	buf := &bytes.Buffer{}
	if resp.ContentLength > 0 {
		buf.Grow(int(resp.ContentLength))
	}
	n, err = io.Copy(buf, resp.Body)
	resp.Body.Close()
	netT = time.Since(netStart)
	if err != nil {
		err = fmt.Errorf("%s: read: %w", path, err)
		return
	}

	tmp := dest + ".part"
	diskStart := time.Now()
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	_, werr := buf.WriteTo(f)
	if werr == nil && doSync {
		werr = f.Sync()
	}
	cerr := f.Close()
	diskT = time.Since(diskStart)
	err = werr
	if err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		err = fmt.Errorf("%s: write: %w", path, err)
		return
	}
	return n, netT, diskT, os.Rename(tmp, dest)
}

func writeResults(path string, rows []row, totalBytes int64, totalNet, totalDisk time.Duration) error {
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
		fmt.Fprintln(f, "file\tbytes\tgib\tnet_s\tdisk_s\tnet_mib_s\tdisk_mib_s")
	}
	for _, r := range rows {
		fmt.Fprintf(f, "%s\t%d\t%.2f\t%.3f\t%.3f\t%.1f\t%.1f\n",
			r.path, r.bytes, float64(r.bytes)/(1<<30),
			r.net.Seconds(), r.disk.Seconds(), mib(r.bytes, r.net), mib(r.bytes, r.disk))
	}
	fmt.Fprintf(f, "TOTAL\t%d\t%.2f\t%.3f\t%.3f\t%.1f\t%.1f\n",
		totalBytes, float64(totalBytes)/(1<<30),
		totalNet.Seconds(), totalDisk.Seconds(), mib(totalBytes, totalNet), mib(totalBytes, totalDisk))
	return nil
}

// listTree paginates the Hub tree API and returns leaf (non-directory) entries.
func listTree(ctx context.Context, repo, rev, token string) ([]entry, error) {
	segments := strings.Split(repo, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	u := fmt.Sprintf("%s/api/models/%s/tree/%s?recursive=true", hub, strings.Join(segments, "/"), rev)

	var files []entry
	for u != "" {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("tree %s: %s: %s", u, resp.Status, body)
		}
		var page []entry
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()
		for _, e := range page {
			if e.Type != "directory" && e.Type != "tree" {
				files = append(files, e)
			}
		}
		u = nextLink(resp.Header.Get("Link"))
	}
	return files, nil
}

func nextLink(link string) string {
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "<")
		if i := strings.Index(part, ">"); i >= 0 {
			return part[:i]
		}
	}
	return ""
}

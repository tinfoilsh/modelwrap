// Command netread measures raw network download throughput: it lists a
// Hugging Face repo's file tree and streams every file to io.Discard — no
// disk writes at all. It is the network-only counterpart of naive: run both
// to see how much disk adds. No Python, no Xet, no concurrency.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
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
	results := flag.String("results", "", "write per-file TSV results to this path")
	flag.Parse()

	if *repo == "" {
		log.Fatal("usage: netread --repo <org/name> [--revision main] [--results file]")
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
	var totalNet time.Duration
	start := time.Now()

	for i, f := range files {
		n, elapsed, err := readToDiscard(ctx, *repo, *rev, f.Path, token)
		if err != nil {
			log.Fatalf("[%d/%d] %s: %v", i+1, len(files), f.Path, err)
		}
		totalBytes += n
		totalNet += elapsed
		log.Printf("[%d/%d] %s: %d bytes %.3fs %.1f MiB/s", i+1, len(files), f.Path, n, elapsed.Seconds(), mib(n, elapsed))
		rows = append(rows, row{f.Path, n, elapsed})
	}

	wall := time.Since(start)
	fmt.Printf("netread: files=%d bytes=%d (%.2f GiB) net=%.3fs wall=%.3fs | net=%.1f MiB/s wall=%.1f MiB/s\n",
		len(rows), totalBytes, float64(totalBytes)/(1<<30),
		totalNet.Seconds(), wall.Seconds(),
		mib(totalBytes, totalNet), float64(totalBytes)/wall.Seconds()/(1<<20))

	if *results != "" {
		if err := writeResults(*results, rows, totalBytes, totalNet); err != nil {
			log.Printf("warning: write results: %v", err)
		}
	}
}

type row struct {
	path    string
	bytes   int64
	elapsed time.Duration
}

func readToDiscard(ctx context.Context, repo, rev, path, token string) (int64, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/%s/resolve/%s/%s", hub, repo, rev, path), nil)
	if err != nil {
		return 0, 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("%s: %s", path, resp.Status)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	return n, time.Since(start), err
}

func writeResults(path string, rows []row, totalBytes int64, totalNet time.Duration) error {
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
		fmt.Fprintln(f, "file\tbytes\tgib\tnet_s\tnet_mib_s")
	}
	for _, r := range rows {
		fmt.Fprintf(f, "%s\t%d\t%.2f\t%.3f\t%.1f\n",
			r.path, r.bytes, float64(r.bytes)/(1<<30), r.elapsed.Seconds(), mib(r.bytes, r.elapsed))
	}
	fmt.Fprintf(f, "TOTAL\t%d\t%.2f\t%.3f\t%.1f\n",
		totalBytes, float64(totalBytes)/(1<<30), totalNet.Seconds(), mib(totalBytes, totalNet))
	return nil
}

func mib(b int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(b) / d.Seconds() / (1 << 20)
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

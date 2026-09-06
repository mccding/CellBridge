package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/modem/at"
	"github.com/cellbridge/cellbridge/gateway/internal/modem/discovery"
)

func main() {
	devRoot := flag.String("device-root", "/dev", "device root to scan")
	port := flag.String("port", "", "explicit serial path; use only when stable discovery is unavailable")
	baud := flag.Int("baud", 9600, "serial baud rate (EC25/QDC507 commonly uses 9600)")
	jsonOutput := flag.Bool("json", false, "write JSON report")
	timeout := flag.Duration("timeout", 3*time.Second, "per AT command timeout")
	flag.Parse()

	candidates, err := candidates(*devRoot, *port)
	if err != nil {
		fatal(err)
	}
	reports := make([]discovery.Report, 0, len(candidates))
	for _, candidate := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		if err := discovery.EnsureDevicePath(candidate.Path); err != nil {
			reports = append(reports, discovery.Report{Candidate: candidate, Error: err.Error()})
			cancel()
			continue
		}
		client, openErr := at.OpenSerial(ctx, candidate.Path, *baud)
		if openErr != nil {
			reports = append(reports, discovery.Report{Candidate: candidate, Error: openErr.Error()})
			cancel()
			continue
		}
		report := discovery.Probe(ctx, candidate, client)
		_ = client.Close()
		reports = append(reports, report)
		cancel()
	}
	if *jsonOutput {
		data, marshalErr := discovery.EncodeReports(reports)
		if marshalErr != nil {
			fatal(marshalErr)
		}
		fmt.Println(string(data))
		return
	}
	for _, report := range reports {
		fmt.Printf("%s\t%s\t%s\t%s\n", report.Path, report.Identity.Model, report.SIM, report.Registration)
		if report.Error != "" {
			fmt.Printf("  error: %s\n", report.Error)
		}
	}
}

func candidates(devRoot, explicitPort string) ([]discovery.Candidate, error) {
	if explicitPort != "" {
		return []discovery.Candidate{{Path: filepath.Clean(explicitPort), Source: "explicit"}}, nil
	}
	return discovery.Enumerate(devRoot)
}

func fatal(err error) {
	data, _ := json.Marshal(map[string]string{"error": err.Error()})
	fmt.Fprintln(os.Stderr, string(data))
	os.Exit(1)
}

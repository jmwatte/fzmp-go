package main

import (
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// track holds a file path plus every CSV column as a generic field map.
// This lets the meta browser treat any column as a drill-down level.
type track struct {
	path   string
	fields map[string]string // keyed by CSV header name
}

// loadCSV reads a foobar2000-style CSV and returns the tracks plus the list
// of non-path column headers (so the UI can show available facets).
func loadCSV(csvPath string) (tracks []track, headers []string, err error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.LazyQuotes = true
	r.TrimLeadingSpace = true

	rawHeaders, err := r.Read()
	if err != nil {
		return nil, nil, err
	}

	// Trim BOM / whitespace from headers
	cols := make([]string, len(rawHeaders))
	for i, h := range rawHeaders {
		cols[i] = strings.TrimSpace(strings.TrimLeft(h, "\xef\xbb\xbf"))
	}

	// Identify the path column (first of these that exists)
	pathCol := -1
	for _, candidate := range []string{"Full Path", "Path", "Filename"} {
		for i, h := range cols {
			if h == candidate {
				pathCol = i
				break
			}
		}
		if pathCol >= 0 {
			break
		}
	}

	// Collect non-path headers for the caller
	for i, h := range cols {
		if i != pathCol {
			headers = append(headers, h)
		}
	}

	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}

		var p string
		if pathCol >= 0 && pathCol < len(row) {
			p = strings.TrimRight(strings.TrimSpace(row[pathCol]), "\r")
		}
		if p == "" {
			continue
		}

		fields := make(map[string]string, len(cols))
		for i, h := range cols {
			if i < len(row) {
				fields[h] = strings.TrimRight(strings.TrimSpace(row[i]), "\r")
			}
		}
		// Ensure Title always has a value
		if fields["Title"] == "" {
			fields["Title"] = filepath.Base(p)
		}
		// Normalise Album Artist fallback
		if fields["Album Artist"] == "" && fields["Artist"] != "" {
			fields["Album Artist"] = fields["Artist"]
		}

		tracks = append(tracks, track{path: p, fields: fields})
	}

	return tracks, headers, nil
}

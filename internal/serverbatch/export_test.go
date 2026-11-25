package serverbatch

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

func TestRedact(t *testing.T) {
	cases := []struct{ in, want string }{
		{"password=hunter2", "password=***"},
		{"PASSWORD: hunter2", "PASSWORD=***"},
		{"passwd=abc", "passwd=***"},
		{"pwd: xyz", "pwd=***"},
		{"token=abc123", "token=***"},
		{"api-key: abc123", "api-key=***"},
		{"api_key=abc123", "api_key=***"},
		{"secret=abc", "secret=***"},
		{"key=abc123", "key=***"},
		{"--password hunter2", "--password ***"},
		{"--password=hunter2", "--password=***"},
		{"--token abc", "--token ***"},
		{"Authorization: Bearer abc.def.ghi", "Authorization: Bearer ***"},
		{"Authorization: Basic dXNlcjpwYXNz", "Authorization: Basic ***"},
		{`password="hunter2"`, `password=***`},
		{"db_password=abc other=keep", "db_password=*** other=keep"},
		// Ordinary text survives untouched.
		{"hello world", "hello world"},
		{"ls -la /tmp", "ls -la /tmp"},
		{"passwords are secret", "passwords are secret"},
		{"monkey=banana", "monkey=banana"},
		{"the token is on the table", "the token is on the table"},
		{"port=8080", "port=8080"},
		{"output: ok", "output: ok"},
	}
	for _, c := range cases {
		if got := Redact(c.in); got != c.want {
			t.Errorf("Redact(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func testRunForExport() *Run {
	exit := 0
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	return &Run{
		ID: "run-export", Command: "echo password=hunter2",
		CreatedAt: now, StartedAt: now, FinishedAt: now.Add(time.Second),
		State: RunFinished,
		Targets: []ServerResult{
			{ServerID: "srv-1", ServerName: "web-1", Address: "10.0.0.1",
				State: StateSuccess, ExitCode: &exit, Output: "token=abc123\nok\n",
				Duration: time.Second},
			{ServerID: "srv-2", ServerName: "web-2", Address: "10.0.0.2",
				State: StateFailed, Error: "拨号失败：connection refused", Duration: time.Second},
		},
	}
}

func TestExportRunsCSV(t *testing.T) {
	var buf bytes.Buffer
	if err := ExportRunsCSV([]*Run{testRunForExport()}, &buf); err != nil {
		t.Fatalf("ExportRunsCSV: %v", err)
	}
	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("reading CSV back: %v", err)
	}
	if len(records) != 3 { // header + 2 servers
		t.Fatalf("CSV has %d rows, want 3", len(records))
	}
	header := records[0]
	if header[0] != "run_id" || header[2] != "command" || header[7] != "state" {
		t.Fatalf("header = %v", header)
	}
	row := records[1]
	if row[0] != "run-export" || row[5] != "web-1" || row[7] != "success" || row[8] != "0" {
		t.Fatalf("row 1 = %v", row)
	}
	// Command and output are redacted in the file.
	if strings.Contains(row[2], "hunter2") || strings.Contains(row[12], "abc123") {
		t.Errorf("CSV leaks credentials: %v", row)
	}
	if !strings.Contains(row[2], "password=***") {
		t.Errorf("CSV command not redacted: %q", row[2])
	}
	if records[2][7] != "failed" {
		t.Errorf("row 2 state = %q, want failed", records[2][7])
	}
	// Empty input still writes a header.
	buf.Reset()
	if err := ExportRunsCSV(nil, &buf); err != nil {
		t.Fatalf("ExportRunsCSV(nil): %v", err)
	}
	if records, _ := csv.NewReader(&buf).ReadAll(); len(records) != 1 {
		t.Errorf("empty export has %d rows, want 1 header", len(records))
	}
}

func TestExportRunsTXT(t *testing.T) {
	var buf bytes.Buffer
	if err := ExportRunsTXT([]*Run{testRunForExport()}, &buf); err != nil {
		t.Fatalf("ExportRunsTXT: %v", err)
	}
	text := buf.String()
	for _, want := range []string{"run-export", "web-1", "web-2", "success", "failed", "password=***"} {
		if !strings.Contains(text, want) {
			t.Errorf("TXT lacks %q:\n%s", want, text)
		}
	}
	for _, leak := range []string{"hunter2", "abc123"} {
		if strings.Contains(text, leak) {
			t.Errorf("TXT leaks %q", leak)
		}
	}
}

func TestExportRunsXLSX(t *testing.T) {
	var buf bytes.Buffer
	if err := ExportRunsXLSX([]*Run{testRunForExport()}, &buf); err != nil {
		t.Fatalf("ExportRunsXLSX: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("xlsx is not a zip: %v", err)
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		var content bytes.Buffer
		_, _ = content.ReadFrom(rc)
		rc.Close()
		files[f.Name] = content.Bytes()
	}
	for _, want := range []string{
		"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml",
		"xl/worksheets/sheet1.xml", "xl/sharedStrings.xml",
	} {
		if _, ok := files[want]; !ok {
			t.Errorf("xlsx lacks %s (has %v)", want, fileNames(zr))
		}
	}
	shared := string(files["xl/sharedStrings.xml"])
	for _, want := range []string{"run-export", "web-1", "web-2", "success", "failed", "password=***"} {
		if !strings.Contains(shared, want) {
			t.Errorf("sharedStrings lacks %q", want)
		}
	}
	for _, leak := range []string{"hunter2", "abc123"} {
		if strings.Contains(shared, leak) {
			t.Errorf("xlsx leaks %q", leak)
		}
	}
	// 3 rows: header + 2 servers.
	sheet := string(files["xl/worksheets/sheet1.xml"])
	if n := strings.Count(sheet, "<row "); n != 3 {
		t.Errorf("sheet has %d rows, want 3", n)
	}
}

func fileNames(zr *zip.Reader) []string {
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names
}

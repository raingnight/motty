package serverbatch

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Export of batch runs (§10.4: CSV, Excel, TXT).
//
// Every exporter redacts the command, every server's output and error
// through Redact before writing, so a credential that travelled in a
// command or in remote output never reaches the exported file in the
// clear. The XLSX writer is hand-written on the standard library only
// (archive/zip + encoding/xml): a minimal workbook with one worksheet
// and a shared-strings table, no new dependency.

var csvHeader = []string{
	"run_id", "created_at", "command", "template_id",
	"server_id", "server_name", "address", "state", "exit_code",
	"duration_ms", "retries", "error", "output",
}

func exitCodeString(code *int) string {
	if code == nil {
		return ""
	}
	return strconv.Itoa(*code)
}

// ExportRunsCSV writes one row per server result, in the runs' order
// (newest run first is the caller's choice; rows keep input order).
func ExportRunsCSV(runs []*Run, w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return err
	}
	for _, run := range runs {
		if run == nil {
			continue
		}
		for _, target := range run.Targets {
			row := []string{
				run.ID,
				run.CreatedAt.Format(time.RFC3339),
				Redact(run.Command),
				run.TemplateID,
				target.ServerID,
				target.ServerName,
				target.Address,
				target.State.String(),
				exitCodeString(target.ExitCode),
				strconv.FormatInt(target.Duration.Milliseconds(), 10),
				strconv.Itoa(target.Retries),
				Redact(target.Error),
				Redact(target.Output),
			}
			if err := cw.Write(row); err != nil {
				return err
			}
		}
	}
	cw.Flush()
	return cw.Error()
}

// ExportRunsTXT writes a plain-text report: one section per run, one
// block per server, redacted.
func ExportRunsTXT(runs []*Run, w io.Writer) error {
	var b strings.Builder
	for i, run := range runs {
		if run == nil {
			continue
		}
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "批量任务 %s\n", run.ID)
		fmt.Fprintf(&b, "创建时间：%s\n", run.CreatedAt.Format("2006-01-02 15:04:05"))
		fmt.Fprintf(&b, "命令：%s\n", Redact(run.Command))
		if run.TemplateID != "" {
			fmt.Fprintf(&b, "模板：%s\n", run.TemplateID)
		}
		fmt.Fprintf(&b, "状态：%s\n", run.State.String())
		for _, target := range run.Targets {
			fmt.Fprintf(&b, "\n服务器：%s (%s)\n", target.ServerName, target.Address)
			fmt.Fprintf(&b, "结果：%s", target.State.String())
			if target.ExitCode != nil {
				fmt.Fprintf(&b, "，退出码 %d", *target.ExitCode)
			}
			fmt.Fprintf(&b, "，耗时 %s", target.Duration.Round(time.Millisecond).String())
			if target.Retries > 0 {
				fmt.Fprintf(&b, "，重试 %d 次", target.Retries)
			}
			b.WriteString("\n")
			if target.Error != "" {
				fmt.Fprintf(&b, "错误：%s\n", Redact(target.Error))
			}
			if target.Output != "" {
				b.WriteString("输出：\n")
				b.WriteString(Redact(target.Output))
				if !strings.HasSuffix(target.Output, "\n") {
					b.WriteString("\n")
				}
			}
			if target.OutputTruncated {
				b.WriteString("（输出已截断）\n")
			}
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// --- minimal XLSX writer ---

// ExportRunsXLSX writes a minimal .xlsx workbook (one worksheet, one
// row per server result plus a header row) using only the standard
// library. The file opens in Excel/LibreOffice; shared strings hold all
// cell text.
func ExportRunsXLSX(runs []*Run, w io.Writer) error {
	// Collect rows and the shared-strings table.
	rows := [][]string{csvHeader}
	for _, run := range runs {
		if run == nil {
			continue
		}
		for _, target := range run.Targets {
			rows = append(rows, []string{
				run.ID,
				run.CreatedAt.Format(time.RFC3339),
				Redact(run.Command),
				run.TemplateID,
				target.ServerID,
				target.ServerName,
				target.Address,
				target.State.String(),
				exitCodeString(target.ExitCode),
				strconv.FormatInt(target.Duration.Milliseconds(), 10),
				strconv.Itoa(target.Retries),
				Redact(target.Error),
				Redact(target.Output),
			})
		}
	}
	shared := map[string]int{}
	var sharedList []string
	intern := func(s string) int {
		if idx, ok := shared[s]; ok {
			return idx
		}
		idx := len(sharedList)
		shared[s] = idx
		sharedList = append(sharedList, s)
		return idx
	}
	for _, row := range rows {
		for _, cell := range row {
			intern(cell)
		}
	}

	zw := zip.NewWriter(w)
	writeFile := func(name, content string) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = io.WriteString(f, content)
		return err
	}
	// Deterministic order keeps the output stable for tests.
	files := []struct{ name, content string }{
		{"[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/><Override PartName="/xl/sharedStrings.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sharedStrings+xml"/></Types>`},
		{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
		{"xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="批量命令历史" sheetId="1" r:id="rId1"/></sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/sharedStrings" Target="sharedStrings.xml"/></Relationships>`},
		{"xl/sharedStrings.xml", sharedStringsXML(sharedList)},
		{"xl/worksheets/sheet1.xml", sheetXML(rows, shared)},
	}
	// sheet1.xml and sharedStrings.xml are built from the tables above.
	for _, f := range files {
		if err := writeFile(f.name, f.content); err != nil {
			_ = zw.Close()
			return err
		}
	}
	return zw.Close()
}

func sharedStringsXML(items []string) string {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	fmt.Fprintf(&b, `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="%d" uniqueCount="%d">`, len(items), len(items))
	for _, s := range items {
		b.WriteString("<si><t>")
		_ = xml.EscapeText(&b, []byte(s))
		b.WriteString("</t></si>")
	}
	b.WriteString("</sst>")
	return b.String()
}

// columnName converts a zero-based column index to an Excel column
// letter (A, B, ..., Z, AA, ...).
func columnName(index int) string {
	name := ""
	for index >= 0 {
		name = string(rune('A'+index%26)) + name
		index = index/26 - 1
	}
	return name
}

func sheetXML(rows [][]string, shared map[string]int) string {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	b.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for r, row := range rows {
		fmt.Fprintf(&b, `<row r="%d">`, r+1)
		for c, cell := range row {
			fmt.Fprintf(&b, `<c r="%s%d" t="s"><v>%d</v></c>`, columnName(c), r+1, shared[cell])
		}
		b.WriteString("</row>")
	}
	b.WriteString("</sheetData></worksheet>")
	return b.String()
}

// sortedKeys is a small helper kept for tests that build expected
// shared-string orders; production code interns in row order.
func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

package serverassets

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Minimal .xlsx reader, standard library only (archive/zip + encoding/xml).
// It reads the first worksheet only and supports shared strings, inline
// strings and numbers. Formulas are not evaluated: the cached value in <v>
// is read instead. There is no xlsx dependency in the module cache, and
// none is added.

// ParseXLSX parses .xlsx data (the whole file in memory). The first row is
// the header; the same column rules as ParseCSV apply, including rejecting
// a credential column for the whole batch.
func ParseXLSX(data []byte) ([]ImportRow, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("serverassets: read xlsx: %w", err)
	}
	return parseXLSXZip(zr)
}

// ParseXLSXReader parses .xlsx data from a ReaderAt of the given size.
func ParseXLSXReader(r io.ReaderAt, size int64) ([]ImportRow, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("serverassets: read xlsx: %w", err)
	}
	return parseXLSXZip(zr)
}

func zipFile(zr *zip.Reader, name string) *zip.File {
	for _, f := range zr.File {
		if f.Name == name {
			return f
		}
	}
	return nil
}

func readZipFile(f *zip.File) ([]byte, error) {
	if f == nil {
		return nil, fmt.Errorf("serverassets: xlsx is missing a required part")
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func parseXLSXZip(zr *zip.Reader) ([]ImportRow, error) {
	shared, err := readSharedStrings(zr)
	if err != nil {
		return nil, err
	}
	sheetName, err := firstSheetName(zr)
	if err != nil {
		return nil, err
	}
	data, err := readZipFile(zipFile(zr, sheetName))
	if err != nil {
		return nil, err
	}
	rows, err := readSheet(data, shared)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: empty file", ErrMissingColumns)
	}
	columns, err := checkHeaders(rows[0])
	if err != nil {
		return nil, err
	}
	var out []ImportRow
	for i, cells := range rows[1:] {
		if rowIsEmpty(cells) {
			continue
		}
		out = append(out, rowFromCells(cells, columns, i+2))
	}
	return out, nil
}

// firstSheetName resolves the first <sheet> in xl/workbook.xml through
// xl/_rels/workbook.xml.rels. It falls back to xl/worksheets/sheet1.xml
// when the workbook parts are absent.
func firstSheetName(zr *zip.Reader) (string, error) {
	workbook := zipFile(zr, "xl/workbook.xml")
	if workbook == nil {
		if zipFile(zr, "xl/worksheets/sheet1.xml") != nil {
			return "xl/worksheets/sheet1.xml", nil
		}
		return "", fmt.Errorf("serverassets: xlsx has no workbook")
	}
	data, err := readZipFile(workbook)
	if err != nil {
		return "", err
	}
	// Collect sheet r:ids in document order.
	var sheetIDs []string
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if start, ok := tok.(xml.StartElement); ok && start.Name.Local == "sheet" {
			for _, attr := range start.Attr {
				if attr.Name.Local == "id" {
					sheetIDs = append(sheetIDs, attr.Value)
				}
			}
		}
	}
	rels := map[string]string{}
	if relsFile := zipFile(zr, "xl/_rels/workbook.xml.rels"); relsFile != nil {
		if relData, err := readZipFile(relsFile); err == nil {
			relDec := xml.NewDecoder(bytes.NewReader(relData))
			for {
				tok, err := relDec.Token()
				if err != nil {
					break
				}
				if start, ok := tok.(xml.StartElement); ok && start.Name.Local == "Relationship" {
					var id, target string
					for _, attr := range start.Attr {
						switch attr.Name.Local {
						case "Id":
							id = attr.Value
						case "Target":
							target = attr.Value
						}
					}
					if id != "" && target != "" {
						rels[id] = target
					}
				}
			}
		}
	}
	for _, id := range sheetIDs {
		target, ok := rels[id]
		if !ok {
			continue
		}
		target = strings.TrimPrefix(target, "/")
		if !strings.HasPrefix(target, "xl/") {
			target = "xl/" + target
		}
		if zipFile(zr, target) != nil {
			return target, nil
		}
	}
	// Fall back to the first worksheet part, in name order.
	var sheets []string
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "xl/worksheets/sheet") && strings.HasSuffix(f.Name, ".xml") {
			sheets = append(sheets, f.Name)
		}
	}
	if len(sheets) == 0 {
		return "", fmt.Errorf("serverassets: xlsx has no worksheet")
	}
	sort.Strings(sheets)
	return sheets[0], nil
}

// readSharedStrings returns the shared-string table: each <si> contributes
// its concatenated <t> text (rich-text runs included).
func readSharedStrings(zr *zip.Reader) ([]string, error) {
	f := zipFile(zr, "xl/sharedStrings.xml")
	if f == nil {
		return nil, nil
	}
	data, err := readZipFile(f)
	if err != nil {
		return nil, err
	}
	var out []string
	dec := xml.NewDecoder(bytes.NewReader(data))
	var current strings.Builder
	inSI, inT := false, false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI, current = true, strings.Builder{}
			case "t":
				if inSI {
					inT = true
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				if inSI {
					out = append(out, current.String())
				}
				inSI = false
			case "t":
				inT = false
			}
		case xml.CharData:
			if inT {
				current.Write([]byte(t))
			}
		}
	}
	return out, nil
}

// columnIndex converts a cell reference ("C5", "AA12") to a 0-based column.
func columnIndex(ref string) int {
	col := 0
	for _, c := range ref {
		if c >= 'A' && c <= 'Z' {
			col = col*26 + int(c-'A') + 1
		} else if c >= 'a' && c <= 'z' {
			col = col*26 + int(c-'a') + 1
		} else {
			break
		}
	}
	return col - 1
}

// readSheet returns the first worksheet as rows of cell strings. A cell's
// value comes from <v> (shared-string index when t="s", cached value for a
// formula) or from <is><t> for an inline string.
func readSheet(data []byte, shared []string) ([][]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var rows [][]string
	var cells []string
	var cellType, cellRef string
	var value strings.Builder
	var inline strings.Builder
	inCell, inV, inIS, inT := false, false, false, false
	flushCell := func() {
		if !inCell {
			return
		}
		idx := columnIndex(cellRef)
		if idx < 0 {
			idx = len(cells)
		}
		for len(cells) <= idx {
			cells = append(cells, "")
		}
		text := strings.TrimSpace(value.String())
		if cellType == "s" {
			if n, err := strconv.Atoi(text); err == nil && n >= 0 && n < len(shared) {
				text = shared[n]
			} else {
				text = ""
			}
		} else if cellType == "inlineStr" {
			text = inline.String()
		}
		cells[idx] = text
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("serverassets: parse worksheet: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				cells = []string{}
			case "c":
				inCell = true
				cellType, cellRef = "", ""
				value.Reset()
				inline.Reset()
				for _, attr := range t.Attr {
					switch attr.Name.Local {
					case "t":
						cellType = attr.Value
					case "r":
						cellRef = attr.Value
					}
				}
			case "v":
				if inCell {
					inV = true
				}
			case "is":
				if inCell {
					inIS = true
				}
			case "t":
				if inIS {
					inT = true
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "row":
				rows = append(rows, cells)
				cells = nil
			case "c":
				flushCell()
				inCell, inV, inIS, inT = false, false, false, false
			case "v":
				inV = false
			case "is":
				inIS = false
			case "t":
				inT = false
			}
		case xml.CharData:
			if inV {
				value.Write([]byte(t))
			} else if inT {
				inline.Write([]byte(t))
			}
		}
	}
	return rows, nil
}

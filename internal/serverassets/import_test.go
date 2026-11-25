package serverassets

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"
)

const importCSV = `name,hostname,address,port,user,auth_method,group,tags,notes,os
web-1,web-1,10.0.0.1,22,root,password,prod,web;eu,first,Ubuntu 24.04
db-1,db-1,10.0.0.2,2222,admin,key,prod,db,,Debian 12
`

func TestImportPreviewAndApplyCSV(t *testing.T) {
	s := openTestStore(t)
	rows, err := ParseCSV(strings.NewReader(importCSV))
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("parsed %d rows, want 2", len(rows))
	}
	preview, err := s.PreviewImport(rows)
	if err != nil {
		t.Fatalf("PreviewImport: %v", err)
	}
	if len(preview.ToAdd) != 2 || len(preview.Skipped) != 0 || len(preview.Failed) != 0 {
		t.Fatalf("preview: toAdd=%d skipped=%d failed=%d", len(preview.ToAdd), len(preview.Skipped), len(preview.Failed))
	}
	if len(preview.NewGroups) != 1 || preview.NewGroups[0] != "prod" {
		t.Fatalf("new groups: %v", preview.NewGroups)
	}
	summary, err := s.ApplyImport(preview)
	if err != nil {
		t.Fatalf("ApplyImport: %v", err)
	}
	if summary.Added != 2 || summary.Skipped != 0 || summary.Failed != 0 {
		t.Fatalf("summary: %+v", summary)
	}
	servers, err := s.ListServers(Filter{})
	if err != nil || len(servers) != 2 {
		t.Fatalf("servers after import: %d, err %v", len(servers), err)
	}
	groups, err := s.ListGroups()
	if err != nil || len(groups) != 1 || groups[0].Name != "prod" {
		t.Fatalf("groups after import: %v, err %v", groups, err)
	}
	if servers[0].GroupID != groups[0].ID {
		t.Fatal("imported server not linked to created group")
	}
	// Re-importing the same file skips both rows and adds nothing.
	preview2, err := s.PreviewImport(rows)
	if err != nil {
		t.Fatalf("second PreviewImport: %v", err)
	}
	if len(preview2.ToAdd) != 0 || len(preview2.Skipped) != 2 {
		t.Fatalf("second preview: toAdd=%d skipped=%d", len(preview2.ToAdd), len(preview2.Skipped))
	}
	summary2, err := s.ApplyImport(preview2)
	if err != nil {
		t.Fatalf("second ApplyImport: %v", err)
	}
	if summary2.Added != 0 || summary2.Skipped != 2 {
		t.Fatalf("second summary: %+v", summary2)
	}
}

func TestImportDuplicateWithinFileAndIPNormalisation(t *testing.T) {
	s := openTestStore(t)
	csvData := "name,hostname,address,port,user\n" +
		"a,host-a,10.0.0.1,22,root\n" +
		"b,host-b,10.0.0.1,22,root\n" + // same IP+port, same file
		"c,host-c,2001:db8::1,22,root\n" +
		"d,host-d,2001:0db8:0000:0000:0000:0000:0000:0001,22,root\n" // IPv6 expanded form
	rows, err := ParseCSV(strings.NewReader(csvData))
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	preview, err := s.PreviewImport(rows)
	if err != nil {
		t.Fatalf("PreviewImport: %v", err)
	}
	if len(preview.ToAdd) != 2 || len(preview.Skipped) != 2 {
		t.Fatalf("preview: toAdd=%d skipped=%d failed=%d", len(preview.ToAdd), len(preview.Skipped), len(preview.Failed))
	}
	summary, err := s.ApplyImport(preview)
	if err != nil {
		t.Fatalf("ApplyImport: %v", err)
	}
	if summary.Added != 2 || summary.Skipped != 2 {
		t.Fatalf("summary: %+v", summary)
	}
}

func TestImportValidationFailuresAndHostnameWarning(t *testing.T) {
	s := openTestStore(t)
	mustCreate(t, s, testServer("existing", "10.0.0.1", 22))
	// Existing server has hostname "host-x" at 10.0.0.1; importing the same
	// hostname at a different IP must warn but still import.
	existing, _ := s.GetServer(mustCreate(t, s, testServer("hx", "10.0.0.9", 22)).ID)
	existing.Hostname = "host-x"
	if _, err := s.UpdateServer(existing); err != nil {
		t.Fatalf("set hostname: %v", err)
	}
	csvData := "name,hostname,address,port,user,auth_method,jump_host\n" +
		",host-a,10.0.0.5,22,root,password,\n" + // missing name
		"bad-port,host-b,10.0.0.6,99999,root,password,\n" + // bad port
		"bad-addr,host-c,not a host!!,22,root,password,\n" + // bad address
		"bad-auth,host-d,10.0.0.7,22,root,cert,\n" + // bad auth method
		"bad-jump,host-e,10.0.0.8,22,root,password,missing-bastion\n" + // unknown jump host
		"same-host,host-x,10.0.0.10,22,root,password,\n" // same hostname, different IP
	rows, err := ParseCSV(strings.NewReader(csvData))
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	preview, err := s.PreviewImport(rows)
	if err != nil {
		t.Fatalf("PreviewImport: %v", err)
	}
	if len(preview.Failed) != 5 {
		t.Fatalf("failed = %d (%v), want 5", len(preview.Failed), preview.Failed)
	}
	if len(preview.Warnings) != 1 || preview.Warnings[0].Name != "same-host" {
		t.Fatalf("warnings = %v, want 1 for same-host", preview.Warnings)
	}
	if len(preview.ToAdd) != 1 {
		t.Fatalf("toAdd = %d, want 1", len(preview.ToAdd))
	}
	summary, err := s.ApplyImport(preview)
	if err != nil {
		t.Fatalf("ApplyImport: %v", err)
	}
	if summary.Added != 1 || summary.Failed != 5 {
		t.Fatalf("summary: %+v", summary)
	}
}

func TestImportRejectsPasswordColumn(t *testing.T) {
	for _, header := range []string{
		"name,address,password\n",
		"name,address,Password\n",
		"name,address,passphrase\n",
		"name,address,private_key\n",
	} {
		if _, err := ParseCSV(strings.NewReader(header + "a,10.0.0.1,x\n")); !errors.Is(err, ErrPasswordColumn) {
			t.Fatalf("header %q: got %v, want ErrPasswordColumn", header, err)
		}
	}
	if _, err := ParseCSV(strings.NewReader("address\n10.0.0.1\n")); !errors.Is(err, ErrMissingColumns) {
		t.Fatalf("missing name column: got %v, want ErrMissingColumns", err)
	}
}

func TestImportJumpHostByName(t *testing.T) {
	s := openTestStore(t)
	jump, err := s.CreateJumpHost(JumpHost{Name: "Bastion", Address: "203.0.113.1", Port: 22, User: "jump"})
	if err != nil {
		t.Fatalf("CreateJumpHost: %v", err)
	}
	rows, err := ParseCSV(strings.NewReader("name,address,port,user,jump_host\na,10.0.0.1,22,root,bastion\n"))
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	preview, err := s.PreviewImport(rows)
	if err != nil {
		t.Fatalf("PreviewImport: %v", err)
	}
	if len(preview.ToAdd) != 1 || preview.ToAdd[0].JumpHostID != jump.ID {
		t.Fatalf("jump host not resolved by name: %+v", preview.ToAdd)
	}
	summary, err := s.ApplyImport(preview)
	if err != nil || summary.Added != 1 {
		t.Fatalf("ApplyImport: summary %+v err %v", summary, err)
	}
}

// buildTestXLSX builds a minimal .xlsx in memory: shared strings, one
// worksheet, plus a formula cell whose cached value is read.
func buildTestXLSX(t *testing.T) []byte {
	return buildTestXLSXWithUserHeader(t, "user")
}

func buildTestXLSXWithUserHeader(t *testing.T, userHeader string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	add := func(name, content string) {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	add("[Content_Types].xml", `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/><Override PartName="/xl/sharedStrings.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sharedStrings+xml"/></Types>`)
	add("xl/workbook.xml", `<?xml version="1.0"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Servers" sheetId="1" r:id="rId1"/></sheets></workbook>`)
	add("xl/_rels/workbook.xml.rels", `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`)
	// shared strings: 0=name 1=address 2=port 3=userHeader 4=xlsx-1 5=10.0.0.20 6=root
	add("xl/sharedStrings.xml", `<?xml version="1.0"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="7" uniqueCount="7"><si><t>name</t></si><si><t>address</t></si><si><t>port</t></si><si><t>`+userHeader+`</t></si><si><t>xlsx-1</t></si><si><t>10.0.0.20</t></si><si><t>root</t></si></sst>`)
	add("xl/worksheets/sheet1.xml", `<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`+
		`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="s"><v>2</v></c><c r="D1" t="s"><v>3</v></c></row>`+
		// Shared-string name/address/user, numeric port, inline-string note cell in E is ignored (unknown column absent).
		`<row r="2"><c r="A2" t="s"><v>4</v></c><c r="B2" t="s"><v>5</v></c><c r="C2"><v>22</v></c><c r="D2" t="s"><v>6</v></c></row>`+
		// Formula cell: cached value 2222 must be read, formula ignored.
		`<row r="3"><c r="A3" t="inlineStr"><is><t>xlsx-2</t></is></c><c r="B3" t="inlineStr"><is><t>10.0.0.21</t></is></c><c r="C3"><f>1+1</f><v>2222</v></c><c r="D3" t="inlineStr"><is><t>admin</t></is></c></row>`+
		`</sheetData></worksheet>`)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImportXLSX(t *testing.T) {
	data := buildTestXLSX(t)
	rows, err := ParseXLSX(data)
	if err != nil {
		t.Fatalf("ParseXLSX: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("parsed %d rows, want 2: %+v", len(rows), rows)
	}
	if rows[0].Name != "xlsx-1" || rows[0].Address != "10.0.0.20" || rows[0].Port != 22 || rows[0].User != "root" {
		t.Fatalf("row 0: %+v", rows[0])
	}
	if rows[1].Name != "xlsx-2" || rows[1].Address != "10.0.0.21" || rows[1].Port != 2222 || rows[1].User != "admin" {
		t.Fatalf("row 1 (inline strings + formula cached value): %+v", rows[1])
	}
	s := openTestStore(t)
	summary, err := s.ImportXLSX(data)
	if err != nil {
		t.Fatalf("ImportXLSX: %v", err)
	}
	if summary.Added != 2 || summary.Skipped != 0 || summary.Failed != 0 {
		t.Fatalf("summary: %+v", summary)
	}
	servers, err := s.ListServers(Filter{})
	if err != nil || len(servers) != 2 {
		t.Fatalf("servers after xlsx import: %d, err %v", len(servers), err)
	}
	// A password column in xlsx must also reject the batch. (The variant
	// is rebuilt, not byte-patched: the zip payload is compressed.)
	bad := buildTestXLSXWithUserHeader(t, "password")
	if _, err := ParseXLSX(bad); !errors.Is(err, ErrPasswordColumn) {
		t.Fatalf("xlsx password column: got %v, want ErrPasswordColumn", err)
	}
}

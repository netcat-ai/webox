package wechatdb

import (
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseFileMessage(t *testing.T) {
	content := "wxid_sender:\n" + `<msg><appmsg><title><![CDATA[测试报告.pdf]]></title><type>6</type>` +
		`<appattach><totallen>1234</totallen></appattach></appmsg></msg>`
	metadata, ok := parseFileMessage(content)
	if !ok || metadata.Filename != "测试报告.pdf" {
		t.Fatalf("metadata=%#v ok=%v", metadata, ok)
	}
	if _, ok := parseFileMessage(`<msg><appmsg><title>link</title><type>5</type></appmsg></msg>`); ok {
		t.Fatal("link app message was treated as a file")
	}
}

func TestResourceDetailFileNames(t *testing.T) {
	packed, err := hex.DecodeString("0A170A08647972642E7A6970120B647972642831292E7A6970")
	if err != nil {
		t.Fatal(err)
	}
	original, stored := resourceDetailFileNames(packed)
	if original != "dyrd.zip" || stored != "dyrd(1).zip" {
		t.Fatalf("original=%q stored=%q", original, stored)
	}
}

func TestFileMessageReadsSubtypeAndMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "message.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `CREATE TABLE [Msg_test] (
        local_id INTEGER PRIMARY KEY, server_id INTEGER, local_type INTEGER,
        create_time INTEGER, message_content BLOB, WCDB_CT_message_content INTEGER
    )`)
	mustExec(t, db, `INSERT INTO [Msg_test] VALUES (?, ?, ?, ?, ?, ?)`,
		7, 456, int64(6)*4294967296+49, 1785837898,
		`<msg><appmsg><title>report.pdf</title><type>6</type><appattach><totallen>42</totallen></appattach></appmsg></msg>`, 0)
	defer func() { _ = db.Close() }()
	localID, createTime, metadata, found, err := fileMessage(db, "Msg_test", 456)
	if err != nil || !found || localID != 7 || createTime != 1785837898 || metadata.Filename != "report.pdf" {
		t.Fatalf("localID=%d createTime=%d metadata=%#v found=%v err=%v", localID, createTime, metadata, found, err)
	}
}

func TestLocalFilePathUsesDownloadedCollisionName(t *testing.T) {
	account := t.TempDir()
	created := time.Date(2026, 8, 4, 18, 0, 0, 0, time.Local)
	directory := filepath.Join(account, "msg", "file", "2026-08")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(directory, "report(1).pdf")
	if err := os.WriteFile(expected, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := localFilePath(account, created.Unix(), "report(1).pdf")
	gotInfo, gotErr := os.Stat(got)
	expectedInfo, expectedErr := os.Stat(expected)
	if gotErr != nil || expectedErr != nil || !os.SameFile(gotInfo, expectedInfo) {
		t.Fatalf("path=%q want=%q gotErr=%v expectedErr=%v", got, expected, gotErr, expectedErr)
	}
	if got := localFilePath(account, created.Unix(), "../secret"); got != "" {
		t.Fatalf("escape path=%q", got)
	}
}

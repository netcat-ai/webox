package wechatdb

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestConversationRemarkReadsTheExplicitContactRemark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contact.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `CREATE TABLE contact (
		username TEXT, nick_name TEXT, remark TEXT, alias TEXT, delete_flag INTEGER
	)`)
	mustExec(t, db,
		"INSERT INTO contact(username, nick_name, remark, alias, delete_flag) VALUES (?, ?, ?, ?, ?)",
		"family@chatroom", "Family", "wb-family", "family", 0,
	)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	remark, err := conversationRemarkFromDB(path, "family@chatroom")
	if err != nil {
		t.Fatal(err)
	}
	if remark != "wb-family" {
		t.Fatalf("remark=%q", remark)
	}
}

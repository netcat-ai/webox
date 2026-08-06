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
		"family@chatroom", "Family", "webox.family", "family", 0,
	)
	defer func() { _ = db.Close() }()

	remark, err := conversationRemarkFromDB(db, "family@chatroom")
	if err != nil {
		t.Fatal(err)
	}
	if remark != "webox.family" {
		t.Fatalf("remark=%q", remark)
	}
}

func TestConversationMetadataPrefersNicknameAndIncludesRemark(t *testing.T) {
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
		"family@chatroom", "Family Chat", "webox.family", "family", 0,
	)
	defer func() { _ = db.Close() }()

	metadata, err := conversationMetadataFromDB(db, "family@chatroom")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Name != "Family Chat" || metadata.Remark != "webox.family" {
		t.Fatalf("metadata=%#v", metadata)
	}
}

func TestAccountInfoReadsVisibleWeChatIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contact.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `CREATE TABLE contact (
		username TEXT, nick_name TEXT, remark TEXT, alias TEXT, delete_flag INTEGER,
		big_head_url TEXT, small_head_url TEXT
	)`)
	mustExec(t, db,
		`INSERT INTO contact(username, nick_name, remark, alias, delete_flag, big_head_url, small_head_url)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"wxid_self", "小鱼", "", "jlyfish", 0, "https://example.test/big.png", "https://example.test/small.png",
	)
	defer func() { _ = db.Close() }()

	info, err := accountInfoFromDB(db, "wxid_self")
	if err != nil {
		t.Fatal(err)
	}
	if info.AccountID != "wxid_self" || info.WeChatID != "jlyfish" || info.Nickname != "小鱼" || info.AvatarURL != "https://example.test/big.png" {
		t.Fatalf("account info=%#v", info)
	}
}

func TestAccountInfoUsesUsernameWhenAliasIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contact.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `CREATE TABLE contact (
		username TEXT, nick_name TEXT, remark TEXT, alias TEXT, delete_flag INTEGER,
		big_head_url TEXT, small_head_url TEXT
	)`)
	mustExec(t, db,
		`INSERT INTO contact(username, nick_name, remark, alias, delete_flag, big_head_url, small_head_url)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"jlyfish", "小金鱼", "", "", 0, "", "https://example.test/small.png",
	)
	defer func() { _ = db.Close() }()

	info, err := accountInfoFromDB(db, "jlyfish")
	if err != nil {
		t.Fatal(err)
	}
	if info.AccountID != "jlyfish" || info.WeChatID != "jlyfish" || info.Nickname != "小金鱼" || info.AvatarURL != "https://example.test/small.png" {
		t.Fatalf("account info=%#v", info)
	}
}

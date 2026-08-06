package wechatdb

import (
	"bytes"
	"crypto/aes"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestNormalizedMessageFlattensQuotedContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "text",
			content: `<msg><appmsg><title><![CDATA[虾虾 回答一下]]></title><type>57</type><refermsg>` +
				`<type>1</type><content><![CDATA[这车油耗多少]]></content></refermsg></appmsg></msg>`,
			want: "虾虾 回答一下\n[引用消息] 这车油耗多少",
		},
		{
			name: "image",
			content: `<msg><appmsg><title><![CDATA[虾虾 看看这个]]></title><type>57</type><refermsg>` +
				`<type>3</type><content><![CDATA[<msg><img aeskey="secret" /></msg>]]></content></refermsg></appmsg></msg>`,
			want: "虾虾 看看这个\n[引用消息][图片]",
		},
		{
			name: "link",
			content: `<msg><appmsg><title><![CDATA[虾虾 总结一下]]></title><type>57</type><refermsg>` +
				`<type>49</type><content><![CDATA[<msg><appmsg><title>文章标题</title>` +
				`<url>https://example.com/article?id=1&amp;from=wechat</url></appmsg></msg>]]></content>` +
				`</refermsg></appmsg></msg>`,
			want: "虾虾 总结一下\n[引用消息][链接] 文章标题\nhttps://example.com/article?id=1&from=wechat",
		},
		{
			name: "malformed reference",
			content: `<msg><appmsg><title><![CDATA[虾虾 回答一下]]></title><type>57</type>` +
				`<refermsg><type>1</type></refermsg></appmsg></msg>`,
			want: "虾虾 回答一下",
		},
		{
			name: "malformed xml",
			content: `<msg><appmsg><title><![CDATA[虾虾 回答一下]]></title><refermsg>` +
				`<type>1</type><content><![CDATA[不完整的引用]]></content>`,
			want: "虾虾 回答一下",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kind, got := normalizedMessage(49, test.content, false)
			if kind != "text" {
				t.Fatalf("kind=%q want text", kind)
			}
			if got != test.want {
				t.Fatalf("content=%q want %q", got, test.want)
			}
		})
	}
}

func TestNormalizedMessageFlattensGroupQuotedContentAfterSenderPrefix(t *testing.T) {
	content := "wxid_sender:\n" +
		`<msg><appmsg><title><![CDATA[虾虾 回答一下]]></title><refermsg>` +
		`<type>1</type><content><![CDATA[群里的问题]]></content></refermsg></appmsg></msg>`

	kind, got := normalizedMessage(49, content, true)
	if kind != "text" || got != "虾虾 回答一下\n[引用消息] 群里的问题" {
		t.Fatalf("normalizedMessage()=(%q, %q)", kind, got)
	}
}

func TestNormalizedMessageRecognizesFileAppMessage(t *testing.T) {
	content := "wxid_sender:\n" + `<msg><appmsg><title>report.pdf</title><type>6</type>` +
		`<appattach><totallen>42</totallen></appattach></appmsg></msg>`
	kind, got := normalizedMessage(int64(6)*4294967296+49, content, true)
	if kind != "file" || got != "[文件] report.pdf" {
		t.Fatalf("normalizedMessage()=(%q, %q)", kind, got)
	}
}

func TestQueryExtractsOrdinaryLinkMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "message.db")
	db := createMessageDB(t, path)
	content := `<msg><appmsg><title><![CDATA[文章标题]]></title><des><![CDATA[文章摘要]]></des>` +
		`<type>5</type><url><![CDATA[https://example.com/article?id=1&from=wechat]]></url></appmsg></msg>`
	mustExec(t, db, `INSERT INTO [Msg_test] (
		local_id, server_id, local_type, create_time, real_sender_id,
		message_content, WCDB_CT_message_content, status, origin_source
	) VALUES (1, 101, 49, 1000, 0, ?, 0, 3, 2)`, content)
	defer func() { _ = db.Close() }()

	events, err := queryNewTable(
		messageShard{relativePath: "message/message_0.db", db: db, table: "Msg_test"},
		"alice", false, MessagePosition{CreateTime: 999}, 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"title": "文章标题", "description": "文章摘要",
		"url": "https://example.com/article?id=1&from=wechat",
	}
	if len(events) != 1 || events[0].message["msgtype"] != "link" ||
		!reflect.DeepEqual(events[0].message["link"], want) {
		t.Fatalf("events=%#v want link=%#v", events, want)
	}
}

func TestQueryEmitsRowsWithoutDirectionOrStatusFiltering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "message.db")
	db := createMessageDB(t, path)
	rows := []struct {
		id, status, origin int
		text               string
	}{
		{id: 1, status: 2, origin: 1, text: "local outgoing"},
		{id: 2, status: 3, origin: 2, text: "remote incoming"},
		{id: 3, status: 4, origin: 2, text: "system state"},
		{id: 4, status: 3, origin: 0, text: "other source"},
	}
	for _, row := range rows {
		mustExec(t, db,
			`INSERT INTO [Msg_test] (local_id, server_id, local_type, create_time, real_sender_id,
			 message_content, WCDB_CT_message_content, status, origin_source) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.id, 100+row.id, 1, 1000, 0, row.text, 0, row.status, row.origin,
		)
	}
	defer func() { _ = db.Close() }()
	events, err := queryNewTable(
		messageShard{relativePath: "message/message_0.db", db: db, table: "Msg_test"},
		"alice", false, MessagePosition{CreateTime: 999}, 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(rows) {
		t.Fatalf("events=%d want %d: %#v", len(events), len(rows), events)
	}
	for index, event := range events {
		text := event.message["text"].(map[string]any)["content"]
		if text != rows[index].text || event.position.LocalID != int64(rows[index].id) {
			t.Fatalf("event[%d]=%#v want text=%q", index, event, rows[index].text)
		}
	}
}

func TestQueryExtractsAtUserIDsFromCompressedMessageSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "message.db")
	db := createMessageDB(t, path)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	source := encoder.EncodeAll([]byte(`<msgsource><atuserlist>wxid-self, wxid-other,wxid-self</atuserlist></msgsource>`), nil)
	encoder.Close()
	mustExec(t, db, `INSERT INTO [Msg_test] (
		local_id, server_id, local_type, create_time, real_sender_id,
		message_content, WCDB_CT_message_content, source, WCDB_CT_source, status, origin_source
	) VALUES (1, 101, 1, 1000, 0, 'hello', 0, ?, 4, 3, 2)`, source)
	defer func() { _ = db.Close() }()

	events, err := queryNewTable(
		messageShard{relativePath: "message/message_0.db", db: db, table: "Msg_test"},
		"group@chatroom", true, MessagePosition{CreateTime: 999}, 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"wxid-self", "wxid-other"}
	if len(events) != 1 || !reflect.DeepEqual(events[0].message["at_user_ids"], want) {
		t.Fatalf("events=%#v want at_user_ids=%v", events, want)
	}
}

func TestQueryResumesByLocalIDWithinSameSecond(t *testing.T) {
	path := filepath.Join(t.TempDir(), "message.db")
	db := createMessageDB(t, path)
	mustExec(t, db, "INSERT INTO [Msg_test] (local_id, server_id, local_type, create_time, real_sender_id, message_content, WCDB_CT_message_content, status, origin_source) VALUES (1, 101, 1, 1000, 0, 'first', 0, 3, 2)")
	mustExec(t, db, "INSERT INTO [Msg_test] (local_id, server_id, local_type, create_time, real_sender_id, message_content, WCDB_CT_message_content, status, origin_source) VALUES (2, 102, 1, 1000, 0, 'second', 0, 3, 2)")
	defer func() { _ = db.Close() }()
	events, err := queryNewTable(
		messageShard{relativePath: "message/message_0.db", db: db, table: "Msg_test"},
		"alice", false, MessagePosition{CreateTime: 1000, LocalID: 1}, 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].position.LocalID != 2 {
		t.Fatalf("unexpected events: %#v", events)
	}
}

func TestQuerySkipsRowsWithoutServerID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "message.db")
	db := createMessageDB(t, path)
	mustExec(t, db, "INSERT INTO [Msg_test] (local_id, server_id, local_type, create_time, real_sender_id, message_content, WCDB_CT_message_content, status, origin_source) VALUES (1, 0, 1, 1000, 0, 'pending', 0, 2, 1)")
	mustExec(t, db, "INSERT INTO [Msg_test] (local_id, server_id, local_type, create_time, real_sender_id, message_content, WCDB_CT_message_content, status, origin_source) VALUES (2, 102, 1, 1000, 0, 'ready', 0, 3, 2)")
	defer func() { _ = db.Close() }()
	events, err := queryNewTable(
		messageShard{relativePath: "message/message_0.db", db: db, table: "Msg_test"},
		"alice", false, MessagePosition{CreateTime: 999}, 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].position.LocalID != 2 {
		t.Fatalf("unexpected events: %#v", events)
	}
}

func TestSQLCipherStoreReadsCommittedWALChanges(t *testing.T) {
	dbDir := t.TempDir()
	sessionDir := filepath.Join(dbDir, "session")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sessionDir, "session.db")
	key := strings.Repeat("21", 32)
	writer, err := sql.Open("sqlite3", path+"?_pragma_key=x'"+key+"'&_pragma_cipher_page_size=4096")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	mustExec(t, writer, "PRAGMA journal_mode=WAL")
	mustExec(t, writer, "PRAGMA wal_autocheckpoint=0")
	mustExec(t, writer, "CREATE TABLE SessionTable (username TEXT PRIMARY KEY, last_timestamp INTEGER)")
	mustExec(t, writer, "INSERT INTO SessionTable VALUES ('alice', 100)")

	store, err := Open(dbDir, map[string]string{"session/session.db": key})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	state, err := store.CurrentSessionState()
	if err != nil || state["alice"] != 100 {
		t.Fatalf("initial state=%v err=%v", state, err)
	}

	mustExec(t, writer, "UPDATE SessionTable SET last_timestamp=200 WHERE username='alice'")
	state, err = store.CurrentSessionState()
	if err != nil || state["alice"] != 200 {
		t.Fatalf("updated state=%v err=%v", state, err)
	}
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("encrypted WAL was not present: info=%v err=%v", info, err)
	}
}

func TestDecodeV2ImageRestoresEncryptedAndXORTails(t *testing.T) {
	key := imageKeyMaterial{xor: 0x5a}
	copy(key.aes[:], []byte("0123456789abcdef"))
	aesPlain := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 'a', 'e', 's'}
	rawPlain := []byte("-raw-")
	xorPlain := []byte("tail")
	padding := aes.BlockSize - len(aesPlain)%aes.BlockSize
	padded := append(bytes.Clone(aesPlain), bytes.Repeat([]byte{byte(padding)}, padding)...)
	block, err := aes.NewCipher(key.aes[:])
	if err != nil {
		t.Fatal(err)
	}
	ciphertext := make([]byte, len(padded))
	for offset := 0; offset < len(padded); offset += aes.BlockSize {
		block.Encrypt(ciphertext[offset:offset+aes.BlockSize], padded[offset:offset+aes.BlockSize])
	}
	encoded := make([]byte, v2ImageHeaderSize)
	copy(encoded, v2ImageMagic)
	binary.LittleEndian.PutUint32(encoded[6:10], uint32(len(aesPlain)))
	binary.LittleEndian.PutUint32(encoded[10:14], uint32(len(xorPlain)))
	encoded = append(encoded, ciphertext...)
	encoded = append(encoded, rawPlain...)
	for _, value := range xorPlain {
		encoded = append(encoded, value^key.xor)
	}
	decoded, err := decodeV2Image(encoded, key)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append(bytes.Clone(aesPlain), rawPlain...), xorPlain...)
	if !bytes.Equal(decoded, want) {
		t.Fatalf("decoded=%x want=%x", decoded, want)
	}
}

func TestResourceMD5PrefersPackedMarker(t *testing.T) {
	value := "0123456789abcdef0123456789abcdef"
	packed := append([]byte("prefix"), []byte{0x12, 0x22, 0x0a, 0x20}...)
	packed = append(packed, value...)
	packed = append(packed, []byte("suffix")...)
	if got := resourceMD5(packed); got != value {
		t.Fatalf("resourceMD5=%q want %q packed=%s", got, value, hex.EncodeToString(packed))
	}
}

func TestAccountIDNormalizationMatchesWechatDirectoryNames(t *testing.T) {
	for path, want := range map[string]string{
		"/tmp/wxid_example_ab12/db_storage": "wxid_example",
		"/tmp/123456_ab12/db_storage":       "123456",
		"/tmp/plain/db_storage":             "plain",
	} {
		if got := AccountIDFromDBDir(path); got != want {
			t.Fatalf("AccountIDFromDBDir(%q)=%q want %q", path, got, want)
		}
	}
}

func TestSearchKeyPatterns(t *testing.T) {
	key, salt := "ab"+string(bytes.Repeat([]byte{'1'}, 62)), string(bytes.Repeat([]byte{'2'}, 32))
	data := []byte("prefix x'" + key + salt + "' suffix")
	matches := searchKeyPatterns(data)
	if len(matches) != 1 || matches[0][0] != key || matches[0][1] != salt {
		t.Fatalf("unexpected matches: %#v", matches)
	}
}

func createMessageDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `CREATE TABLE [Msg_test] (
        local_id INTEGER PRIMARY KEY, server_id INTEGER, local_type INTEGER,
        create_time INTEGER, real_sender_id INTEGER, message_content BLOB,
		WCDB_CT_message_content INTEGER, source BLOB, WCDB_CT_source INTEGER,
		status INTEGER, origin_source INTEGER
    )`)
	return db
}

func mustExec(t *testing.T, db *sql.DB, query string, arguments ...any) {
	t.Helper()
	if _, err := db.Exec(query, arguments...); err != nil {
		t.Fatal(err)
	}
}

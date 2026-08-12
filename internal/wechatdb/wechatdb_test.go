package wechatdb

import (
	"bytes"
	"crypto/aes"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/netcat-ai/webox/wecom"
)

func TestNormalizedMessageMapsQuotedContentToReply(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantMsgType string
	}{
		{
			name: "text",
			content: `<msg><appmsg><title><![CDATA[虾虾 回答一下]]></title><type>57</type><refermsg>` +
				`<type>1</type><content><![CDATA[这车油耗多少]]></content></refermsg></appmsg></msg>`,
			wantMsgType: wecom.MessageTypeText,
		},
		{
			name: "image",
			content: `<msg><appmsg><title><![CDATA[虾虾 看看这个]]></title><type>57</type><refermsg>` +
				`<type>3</type><content><![CDATA[<msg><img aeskey="secret" /></msg>]]></content></refermsg></appmsg></msg>`,
			wantMsgType: wecom.MessageTypeImage,
		},
		{
			name: "link",
			content: `<msg><appmsg><title><![CDATA[虾虾 总结一下]]></title><type>57</type><refermsg>` +
				`<type>49</type><content><![CDATA[<msg><appmsg><title>文章标题</title>` +
				`<url>https://example.com/article?id=1&amp;from=wechat</url></appmsg></msg>]]></content>` +
				`</refermsg></appmsg></msg>`,
			wantMsgType: wecom.MessageTypeLink,
		},
		{
			name: "finder feed",
			content: `<msg><appmsg><title><![CDATA[虾虾 看看这个]]></title><type>57</type><refermsg>` +
				`<type>49</type><content><![CDATA[<msg><appmsg><type>51</type><finderFeed>` +
				`<nickname>黄同学的移动小屋</nickname><desc>自驾游装备收纳清单</desc>` +
				`<mediaList><media><url>https://example.com/video?id=1&amp;from=finder</url></media></mediaList>` +
				`</finderFeed></appmsg></msg>]]></content></refermsg></appmsg></msg>`,
			wantMsgType: wecom.MessageTypeSphFeed,
		},
		{
			name: "malformed reference",
			content: `<msg><appmsg><title><![CDATA[虾虾 回答一下]]></title><type>57</type>` +
				`<refermsg><type>1</type></refermsg></appmsg></msg>`,
			wantMsgType: wecom.MessageTypeText,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := wecom.Message{}
			normalizeMessage(&message, 49, test.content, false)
			if message.MsgType != wecom.MessageTypeReply || message.Reply == nil {
				t.Fatalf("message=%#v want reply", message)
			}
			if message.Reply.Parent.MsgType != test.wantMsgType {
				t.Fatalf("parent msgtype=%q want %q", message.Reply.Parent.MsgType, test.wantMsgType)
			}
		})
	}
}

func TestNormalizedMessageFallsBackToTextForMalformedQuotedXML(t *testing.T) {
	content := `<msg><appmsg><title><![CDATA[虾虾 回答一下]]></title><refermsg>` +
		`<type>1</type><content><![CDATA[不完整的引用]]></content>`
	message := wecom.Message{}
	normalizeMessage(&message, 49, content, false)
	if message.MsgType != wecom.MessageTypeText || message.Text == nil || message.Text.Content != "虾虾 回答一下" {
		t.Fatalf("normalizeMessage()=%#v", message)
	}
}

func TestNormalizedMessageMapsGroupQuotedContentAfterSenderPrefix(t *testing.T) {
	content := "wxid_sender:\n" +
		`<msg><appmsg><title><![CDATA[虾虾 回答一下]]></title><refermsg>` +
		`<type>1</type><svrid>123</svrid><fromusr>group@chatroom</fromusr>` +
		`<chatusr>wxid_parent</chatusr><content><![CDATA[群里的问题]]></content></refermsg></appmsg></msg>`

	message := wecom.Message{}
	normalizeMessage(&message, 49, content, true)
	if message.MsgType != wecom.MessageTypeReply || message.Reply == nil || message.Reply.Title != "虾虾 回答一下" ||
		message.Reply.Parent.MsgID != "123" || message.Reply.Parent.From != "wxid_parent" {
		t.Fatalf("normalizeMessage()=%#v", message)
	}
}

func TestNormalizedMessageUsesReplyReferenceForQuotedImage(t *testing.T) {
	content := `<msg><appmsg><title><![CDATA[虾虾 看看这个]]></title><type>57</type><refermsg>` +
		`<type>3</type><svrid>3143822696652695030</svrid><fromusr>group@chatroom</fromusr>` +
		`<chatusr>wxid_sender</chatusr><displayname>小鱼</displayname><createtime>1781703356</createtime>` +
		`<content><![CDATA[<msg><img aeskey="secret" /></msg>]]></content></refermsg></appmsg></msg>`

	message := wecom.Message{}
	normalizeMessage(&message, 49, content, true)
	if message.MsgType != wecom.MessageTypeReply || message.Reply == nil || message.Reply.Title != "虾虾 看看这个" ||
		message.Reply.Parent.MsgID != "3143822696652695030" || message.Reply.Parent.From != "wxid_sender" ||
		message.Reply.Parent.MsgType != wecom.MessageTypeImage || message.Reply.Parent.MsgTime != 1781703356000 {
		t.Fatalf("message=%#v", message)
	}
}

func TestNormalizedMessageRecognizesFileAppMessage(t *testing.T) {
	content := "wxid_sender:\n" + `<msg><appmsg><title>report.pdf</title><type>6</type>` +
		`<appattach><totallen>42</totallen></appattach></appmsg></msg>`
	message := wecom.Message{}
	normalizeMessage(&message, int64(6)*4294967296+49, content, true)
	if message.MsgType != wecom.MessageTypeFile || message.File == nil ||
		message.File.FileName != "report.pdf" || message.File.FileExt != "pdf" {
		t.Fatalf("normalizeMessage()=%#v", message)
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

	messages := queryTestMessages(t, db, "alice", 0)
	if len(messages) != 1 {
		t.Fatalf("messages=%#v want one link item", messages)
	}
	message := &messages[0]
	if message.MsgType != wecom.MessageTypeLink || message.Link == nil || message.Text != nil ||
		message.Link.Title != "文章标题" || message.Link.Description != "文章摘要" ||
		message.Link.LinkURL != "https://example.com/article?id=1&from=wechat" {
		t.Fatalf("message=%#v want typed link", message)
	}
}

func TestQueryExtractsFinderFeedMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "message.db")
	db := createMessageDB(t, path)
	content := `<?xml version="1.0"?><msg><appmsg>` +
		`<title>当前微信版本不支持展示该内容，请升级至最新版本。</title>` +
		`<type>51</type><url>https://support.weixin.qq.com/update/</url>` +
		`<finderFeed><objectId><![CDATA[14919669148928379072]]></objectId>` +
		`<feedType><![CDATA[4]]></feedType><nickname><![CDATA[黄同学的移动小屋]]></nickname>` +
		`<desc><![CDATA[自驾游装备收纳清单
#自驾游 #床车旅行 #床车改装 #床车露营]]></desc>` +
		`<mediaList><media><url><![CDATA[https://wxapp.tc.qq.com/video?id=1&from=finder]]></url>` +
		`<coverUrl><![CDATA[https://wxapp.tc.qq.com/cover?id=1]]></coverUrl>` +
		`<mediaType><![CDATA[4]]></mediaType></media></mediaList>` +
		`</finderFeed></appmsg></msg>`
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll([]byte(content), nil)
	encoder.Close()
	mustExec(t, db, `INSERT INTO [Msg_test] (
		local_id, server_id, local_type, create_time, real_sender_id,
		message_content, WCDB_CT_message_content, status, origin_source
	) VALUES (1, 101, ?, 1000, 0, ?, 4, 3, 2)`, int64(51)<<32|49, compressed)
	defer func() { _ = db.Close() }()

	messages := queryTestMessages(t, db, "alice", 0)
	if len(messages) != 1 {
		t.Fatalf("messages=%#v want one message", messages)
	}
	message := &messages[0]
	if message.MsgType != wecom.MessageTypeSphFeed || message.SphFeed == nil || message.Text != nil {
		t.Fatalf("message=%#v want typed sphfeed item", message)
	}
	feed := message.SphFeed
	if feed.FeedType != 4 || feed.SphName != "黄同学的移动小屋" ||
		feed.FeedDesc != "自驾游装备收纳清单\n#自驾游 #床车旅行 #床车改装 #床车露营" {
		t.Fatalf("sphfeed=%#v", feed)
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
	messages := queryTestMessages(t, db, "alice", 0)
	if len(messages) != len(rows) {
		t.Fatalf("messages=%d want %d: %#v", len(messages), len(rows), messages)
	}
	for index, message := range messages {
		text := message.Text.Content
		if text != rows[index].text || message.Sequence != int64(rows[index].id) {
			t.Fatalf("message[%d]=%#v want text=%q", index, message, rows[index].text)
		}
		if message.Outgoing != (rows[index].origin == 1) {
			t.Fatalf("message[%d] outgoing=%t want %t", index, message.Outgoing, rows[index].origin == 1)
		}
		if message.RoomID != "alice" || len(message.ToList) != 0 {
			t.Fatalf("message[%d] roomid=%q tolist=%#v", index, message.RoomID, message.ToList)
		}
	}
}

func TestQueryResumesByLocalIDWithinSameSecond(t *testing.T) {
	path := filepath.Join(t.TempDir(), "message.db")
	db := createMessageDB(t, path)
	mustExec(t, db, "INSERT INTO [Msg_test] (local_id, server_id, local_type, create_time, real_sender_id, message_content, WCDB_CT_message_content, status, origin_source) VALUES (1, 101, 1, 1000, 0, 'first', 0, 3, 2)")
	mustExec(t, db, "INSERT INTO [Msg_test] (local_id, server_id, local_type, create_time, real_sender_id, message_content, WCDB_CT_message_content, status, origin_source) VALUES (2, 102, 1, 1000, 0, 'second', 0, 3, 2)")
	defer func() { _ = db.Close() }()
	records, err := queryMessageRecords(messageShard{db: db, table: "Msg_test"}, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].localID != 2 {
		t.Fatalf("unexpected records: %#v", records)
	}
}

func TestQueryResumesByLocalIDWhenTimestampMovesBackward(t *testing.T) {
	path := filepath.Join(t.TempDir(), "message.db")
	db := createMessageDB(t, path)
	mustExec(t, db, "INSERT INTO [Msg_test] (local_id, server_id, local_type, create_time, real_sender_id, message_content, WCDB_CT_message_content, status, origin_source) VALUES (1, 101, 1, 1001, 0, 'first', 0, 3, 2)")
	mustExec(t, db, "INSERT INTO [Msg_test] (local_id, server_id, local_type, create_time, real_sender_id, message_content, WCDB_CT_message_content, status, origin_source) VALUES (2, 102, 1, 999, 0, 'second', 0, 3, 2)")
	defer func() { _ = db.Close() }()
	records, err := queryMessageRecords(messageShard{db: db, table: "Msg_test"}, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].localID != 2 {
		t.Fatalf("unexpected records: %#v", records)
	}
}

func TestMaxMessagePositionUsesLatestLocalID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "message.db")
	db := createMessageDB(t, path)
	mustExec(t, db, "INSERT INTO [Msg_test] (local_id, server_id, local_type, create_time, real_sender_id, message_content, WCDB_CT_message_content, status, origin_source) VALUES (1, 101, 1, 1001, 0, 'first', 0, 3, 2)")
	mustExec(t, db, "INSERT INTO [Msg_test] (local_id, server_id, local_type, create_time, real_sender_id, message_content, WCDB_CT_message_content, status, origin_source) VALUES (2, 102, 1, 999, 0, 'second', 0, 3, 2)")
	defer func() { _ = db.Close() }()
	position, found, err := maxMessagePosition(db, "Msg_test")
	if err != nil {
		t.Fatal(err)
	}
	if !found || position.LocalID != 2 || position.CreateTime != 999 {
		t.Fatalf("position=%#v found=%t", position, found)
	}
}

func TestQuerySkipsRowsWithoutServerID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "message.db")
	db := createMessageDB(t, path)
	mustExec(t, db, "INSERT INTO [Msg_test] (local_id, server_id, local_type, create_time, real_sender_id, message_content, WCDB_CT_message_content, status, origin_source) VALUES (1, 0, 1, 1000, 0, 'pending', 0, 2, 1)")
	mustExec(t, db, "INSERT INTO [Msg_test] (local_id, server_id, local_type, create_time, real_sender_id, message_content, WCDB_CT_message_content, status, origin_source) VALUES (2, 102, 1, 1000, 0, 'ready', 0, 3, 2)")
	defer func() { _ = db.Close() }()
	records, err := queryMessageRecords(messageShard{db: db, table: "Msg_test"}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].localID != 2 {
		t.Fatalf("unexpected records: %#v", records)
	}
}

func TestConvertMessagesResolvesSendersByShardAndFallsBackForMissingMappings(t *testing.T) {
	firstDB := createMessageDB(t, filepath.Join(t.TempDir(), "message-0.db"))
	secondDB := createMessageDB(t, filepath.Join(t.TempDir(), "message-1.db"))
	defer func() { _ = firstDB.Close() }()
	defer func() { _ = secondDB.Close() }()
	mustExec(t, firstDB, "INSERT INTO Name2Id (rowid, user_name) VALUES (7, 'wxid-first')")
	mustExec(t, secondDB, "INSERT INTO Name2Id (rowid, user_name) VALUES (7, 'wxid-second')")
	first, err := convertMessages(messageShard{relativePath: "message/message_0.db", db: firstDB}, "first-room", []messageRecord{
		{serverID: 101, realSenderID: 7}, {serverID: 102, realSenderID: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := convertMessages(messageShard{relativePath: "message/message_1.db", db: secondDB}, "second-room", []messageRecord{{serverID: 103, realSenderID: 7}})
	if err != nil {
		t.Fatal(err)
	}
	if first[0].From != "wxid-first" || second[0].From != "wxid-second" {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	if first[1].From != "8" {
		t.Fatalf("fallback message=%#v", first[1])
	}
}

func TestConvertMessagesReturnsName2IDQueryFailure(t *testing.T) {
	db := createMessageDB(t, filepath.Join(t.TempDir(), "message.db"))
	defer func() { _ = db.Close() }()
	mustExec(t, db, "DROP TABLE Name2Id")
	_, err := convertMessages(
		messageShard{relativePath: "message/message_0.db", db: db}, "room",
		[]messageRecord{{serverID: 101, realSenderID: 7}},
	)
	if err == nil || !strings.Contains(err.Error(), "Name2Id") {
		t.Fatalf("convertMessages() error=%v", err)
	}
}

func TestEnabledRoomSessionsUsesRemarkAndLatestLocalID(t *testing.T) {
	contact, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "contact.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = contact.Close() }()
	session, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	mustExec(t, contact, "CREATE TABLE contact (username TEXT, remark TEXT, delete_flag INTEGER)")
	mustExec(t, contact, "INSERT INTO contact VALUES ('enabled', 'webox.jishu', 0), ('enabled-2', 'webox.test', 0), ('spaced', '  webox.invalid  ', 0), ('disabled', 'friend', 0), ('deleted', 'webox.old', 1)")
	mustExec(t, session, "CREATE TABLE SessionTable (username TEXT, last_msg_locald_id INTEGER)")
	mustExec(t, session, "INSERT INTO SessionTable VALUES ('enabled', 123), ('enabled-2', 321), ('spaced', 654), ('disabled', 456), ('deleted', 789), ('missing', 999)")

	store := &Store{dbs: map[string]*sql.DB{
		"contact/contact.db": contact,
		"session/session.db": session,
	}}
	sessions, err := store.EnabledRoomSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions["enabled"] != 123 || sessions["enabled-2"] != 321 {
		t.Fatalf("sessions=%v", sessions)
	}
	mustExec(t, contact, "INSERT INTO contact VALUES ('new', 'webox.new', 0)")
	mustExec(t, session, "UPDATE SessionTable SET last_msg_locald_id=124 WHERE username='enabled'")
	mustExec(t, session, "INSERT INTO SessionTable VALUES ('new', 999)")

	sessions, err = store.EnabledRoomSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions["enabled"] != 124 {
		t.Fatalf("cached sessions=%v", sessions)
	}
	store.enabledRoomsExpires = time.Time{}
	sessions, err = store.EnabledRoomSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 3 || sessions["new"] != 999 {
		t.Fatalf("refreshed sessions=%v", sessions)
	}
}

func TestEnabledRoomSessionsSkipsSessionQueryWithoutEnabledContacts(t *testing.T) {
	contact, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "contact.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = contact.Close() }()
	mustExec(t, contact, "CREATE TABLE contact (username TEXT, remark TEXT, delete_flag INTEGER)")

	store := &Store{dbs: map[string]*sql.DB{"contact/contact.db": contact}}
	sessions, err := store.EnabledRoomSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("sessions=%v", sessions)
	}
}

func TestSQLCipherStoreReadsCommittedWALChanges(t *testing.T) {
	dbDir := t.TempDir()
	contactDir := filepath.Join(dbDir, "contact")
	if err := os.MkdirAll(contactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	contactKey := strings.Repeat("20", 32)
	contact, err := sql.Open("sqlite3", filepath.Join(contactDir, "contact.db")+"?_pragma_key=x'"+contactKey+"'&_pragma_cipher_page_size=4096")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = contact.Close() }()
	mustExec(t, contact, "CREATE TABLE contact (username TEXT PRIMARY KEY, remark TEXT, delete_flag INTEGER)")
	mustExec(t, contact, "INSERT INTO contact VALUES ('alice', 'webox.alice', 0)")

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
	mustExec(t, writer, "CREATE TABLE SessionTable (username TEXT PRIMARY KEY, last_msg_locald_id INTEGER)")
	mustExec(t, writer, "INSERT INTO SessionTable VALUES ('alice', 100)")

	store, err := Open(dbDir, map[string]string{"contact/contact.db": contactKey, "session/session.db": key})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	state, err := store.EnabledRoomSessions()
	if err != nil || state["alice"] != 100 {
		t.Fatalf("initial sessions=%v err=%v", state, err)
	}

	mustExec(t, writer, "UPDATE SessionTable SET last_msg_locald_id=200 WHERE username='alice'")
	state, err = store.EnabledRoomSessions()
	if err != nil || state["alice"] != 200 {
		t.Fatalf("updated sessions=%v err=%v", state, err)
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
	mustExec(t, db, "CREATE TABLE Name2Id (user_name TEXT)")
	return db
}

func queryTestMessages(t *testing.T, db *sql.DB, roomID string, after int64) []wecom.Message {
	t.Helper()
	shard := messageShard{relativePath: "message/message_0.db", db: db, table: "Msg_test"}
	records, err := queryMessageRecords(shard, after, 100)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := convertMessages(shard, roomID, records)
	if err != nil {
		t.Fatal(err)
	}
	return messages
}

func mustExec(t *testing.T, db *sql.DB, query string, arguments ...any) {
	t.Helper()
	if _, err := db.Exec(query, arguments...); err != nil {
		t.Fatal(err)
	}
}

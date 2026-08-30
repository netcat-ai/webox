package wecomarchive

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/netcat-ai/webox/internal/sharedmedia"
)

type fakeClient struct {
	after      uint64
	limit      uint64
	chatData   []ChatData
	chatErr    error
	plain      map[string][]byte
	decryptErr error
	media      map[string][]*MediaData
	mediaCalls map[string]int
	freed      bool
}

func (client *fakeClient) GetChatData(after, limit uint64) ([]ChatData, error) {
	client.after = after
	client.limit = limit
	return client.chatData, client.chatErr
}

func (client *fakeClient) DecryptData(data ChatData) ([]byte, error) {
	return client.plain[data.MessageID], client.decryptErr
}

func (client *fakeClient) GetMediaData(_ string, sdkFileID string) (*MediaData, error) {
	index := client.mediaCalls[sdkFileID]
	client.mediaCalls[sdkFileID]++
	return client.media[sdkFileID][index], nil
}

func (client *fakeClient) Free() {
	client.freed = true
}

func TestGetUpdatesUsesLargestSequenceAndProjectsRooms(t *testing.T) {
	client := &fakeClient{
		chatData: []ChatData{
			{Sequence: 30, MessageID: "group"},
			{Sequence: 32, MessageID: "incoming"},
			{Sequence: 35, MessageID: "outgoing"},
		},
		plain: map[string][]byte{
			"group":    []byte(`{"msgid":"group","action":"send","from":"bob","tolist":["alice"],"roomid":"wr-group","msgtime":1,"msgtype":"text","text":{"content":"group"}}`),
			"incoming": []byte(`{"msgid":"incoming","action":"send","from":"bob","tolist":["alice"],"msgtime":2,"msgtype":"link","link":{"title":"link","link_url":"https://example.com"}}`),
			"outgoing": []byte(`{"msgid":"outgoing","action":"send","from":"alice","tolist":["carol"],"msgtime":3,"msgtype":"text","text":{"content":"hello"}}`),
		},
		mediaCalls: map[string]int{},
	}
	source := newTestSource(t, client, 5)
	updates, next, err := source.GetUpdates(context.Background(), 27, 100)
	if err != nil {
		t.Fatal(err)
	}
	if client.after != 27 || client.limit != 100 {
		t.Fatalf("after=%d limit=%d", client.after, client.limit)
	}
	decoded := decodeUpdates(t, updates)
	if next != 35 || len(decoded) != 3 || decoded[0]["roomid"] != "wr-group@chatroom" || decoded[1]["roomid"] != "bob" || decoded[2]["roomid"] != "carol" {
		t.Fatalf("next=%d updates=%+v", next, decoded)
	}
}

func TestGetUpdatesUsesInitialSequenceWithoutRooms(t *testing.T) {
	client := &fakeClient{plain: map[string][]byte{}, mediaCalls: map[string]int{}}
	source := newTestSource(t, client, 42)
	if _, _, err := source.GetUpdates(context.Background(), 0, 100); err != nil {
		t.Fatal(err)
	}
	if client.after != 42 {
		t.Fatalf("after=%d", client.after)
	}
}

func TestGetUpdatesDownloadsImageAndFileToSharedDirectory(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("image")...)
	fileData := []byte("report")
	imageDigest := md5.Sum(png)
	fileDigest := md5.Sum(fileData)
	client := &fakeClient{
		chatData: []ChatData{{Sequence: 1, MessageID: "image"}, {Sequence: 2, MessageID: "file"}},
		plain: map[string][]byte{
			"image": []byte(`{"msgid":"image","action":"send","from":"bob","tolist":["alice"],"msgtime":1,"msgtype":"image","image":{"sdkfileid":"remote-image","md5sum":"` + hex.EncodeToString(imageDigest[:]) + `","filesize":` + strconv.Itoa(len(png)) + `}}`),
			"file":  []byte(`{"msgid":"file","action":"send","from":"bob","tolist":["alice"],"msgtime":2,"msgtype":"file","file":{"sdkfileid":"remote-file","md5sum":"` + hex.EncodeToString(fileDigest[:]) + `","filename":"report.txt","fileext":"txt","filesize":` + strconv.Itoa(len(fileData)) + `}}`),
		},
		media: map[string][]*MediaData{
			"remote-image": {{NextIndex: "next", Data: png[:5]}, {Finished: true, Data: png[5:]}},
			"remote-file":  {{Finished: true, Data: fileData}},
		},
		mediaCalls: map[string]int{},
	}
	root := t.TempDir()
	store, err := sharedmedia.New(root)
	if err != nil {
		t.Fatal(err)
	}
	source, err := New(client, store, Config{AccountID: "alice", WeChatID: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	updates, _, err := source.GetUpdates(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if calls := client.mediaCalls; !reflect.DeepEqual(calls, map[string]int{"remote-image": 2, "remote-file": 1}) {
		t.Fatalf("media calls=%v", calls)
	}
	decoded := decodeUpdates(t, updates)
	imagePath := decoded[0]["image"].(map[string]any)["sdkfileid"].(string)
	filePath := decoded[1]["file"].(map[string]any)["sdkfileid"].(string)
	for path, expected := range map[string][]byte{imagePath: png, filePath: fileData} {
		contents, err := os.ReadFile(filepath.Join(root, "rooms", "bob", filepath.FromSlash(path)))
		if err != nil || !reflect.DeepEqual(contents, expected) {
			t.Fatalf("path=%q contents=%q err=%v", path, contents, err)
		}
	}
	if imagePath == "remote-image" || filePath == "remote-file" {
		t.Fatalf("remote sdkfileid leaked: image=%q file=%q", imagePath, filePath)
	}
}

func TestGetUpdatesReturnsActualErrors(t *testing.T) {
	client := &fakeClient{chatErr: errors.New("sdk failed"), mediaCalls: map[string]int{}}
	source := newTestSource(t, client, 0)
	if _, _, err := source.GetUpdates(context.Background(), 0, 100); !errors.Is(err, client.chatErr) {
		t.Fatalf("error=%v", err)
	}
}

func TestGetUpdatesPreservesUnknownMessageBodies(t *testing.T) {
	client := &fakeClient{
		chatData: []ChatData{{Sequence: 1, MessageID: "emotion"}, {Sequence: 2, MessageID: "text"}},
		plain: map[string][]byte{
			"emotion": []byte(`{"msgid":"emotion","action":"send","from":"bob","tolist":["alice"],"msgtime":1,"msgtype":"emotion","emotion":{"type":1,"width":120}}`),
			"text":    []byte(`{"msgid":"text","action":"send","from":"bob","tolist":["alice"],"msgtime":2,"msgtype":"text","text":{"content":"keep"}}`),
		},
		mediaCalls: map[string]int{},
	}
	source := newTestSource(t, client, 0)
	updates, _, err := source.GetUpdates(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 2 || !bytes.Contains(updates[0], []byte(`"emotion":{"type":1,"width":120}`)) {
		t.Fatalf("updates=%s", updates)
	}
}

func TestGetUpdatesMaterializesMediaInsideUnknownMessageBody(t *testing.T) {
	gif := []byte("GIF89aunknown-emotion")
	digest := md5.Sum(gif)
	client := &fakeClient{
		chatData: []ChatData{{Sequence: 1, MessageID: "emotion"}},
		plain: map[string][]byte{
			"emotion": []byte(`{"msgid":"emotion","action":"send","from":"bob","tolist":["alice"],"msgtime":1,"msgtype":"emotion","emotion":{"type":1,"sdkfileid":"remote-emotion","md5sum":"` + hex.EncodeToString(digest[:]) + `","imagesize":` + strconv.Itoa(len(gif)) + `}}`),
		},
		media:      map[string][]*MediaData{"remote-emotion": {{Finished: true, Data: gif}}},
		mediaCalls: map[string]int{},
	}
	root := t.TempDir()
	store, err := sharedmedia.New(root)
	if err != nil {
		t.Fatal(err)
	}
	source, err := New(client, store, Config{AccountID: "alice", WeChatID: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	updates, _, err := source.GetUpdates(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeUpdates(t, updates)
	sdkFileID := decoded[0]["emotion"].(map[string]any)["sdkfileid"].(string)
	if sdkFileID == "" || sdkFileID == "remote-emotion" {
		t.Fatalf("sdkfileid=%q", sdkFileID)
	}
	contents, err := os.ReadFile(filepath.Join(root, "rooms", "bob", filepath.FromSlash(sdkFileID)))
	if err != nil || !bytes.Equal(contents, gif) {
		t.Fatalf("contents=%q err=%v", contents, err)
	}
}

func TestGetUpdatesSkipsDirectMessagesForOtherAccounts(t *testing.T) {
	client := &fakeClient{
		chatData: []ChatData{
			{Sequence: 1, MessageID: "unrelated"},
			{Sequence: 2, MessageID: "incoming"},
		},
		plain: map[string][]byte{
			"unrelated": []byte(`{"msgid":"unrelated","action":"send","from":"bob","tolist":["carol"],"msgtime":1,"msgtype":"text","text":{"content":"skip"}}`),
			"incoming":  []byte(`{"msgid":"incoming","action":"send","from":"bob","tolist":["alice"],"msgtime":2,"msgtype":"text","text":{"content":"keep"}}`),
		},
		mediaCalls: map[string]int{},
	}
	source := newTestSource(t, client, 0)
	updates, next, err := source.GetUpdates(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeUpdates(t, updates)
	if next != 2 || len(decoded) != 1 || decoded[0]["msgid"] != "incoming" {
		t.Fatalf("next=%d updates=%+v", next, decoded)
	}
}

func TestGetUpdatesHonorsCancelledContextBeforeCallingSDK(t *testing.T) {
	client := &fakeClient{plain: map[string][]byte{}, mediaCalls: map[string]int{}}
	source := newTestSource(t, client, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := source.GetUpdates(ctx, 0, 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if client.limit != 0 {
		t.Fatal("SDK was called after cancellation")
	}
}

func newTestSource(t *testing.T, client Client, initialSequence uint64) *Source {
	t.Helper()
	media, err := sharedmedia.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, err := New(client, media, Config{AccountID: "alice", WeChatID: "archive", InitialSequence: initialSequence})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func decodeUpdates(t *testing.T, updates []json.RawMessage) []map[string]any {
	t.Helper()
	result := make([]map[string]any, len(updates))
	for index := range updates {
		if err := json.Unmarshal(updates[index], &result[index]); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

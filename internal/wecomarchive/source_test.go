package wecomarchive

import (
	"context"
	"crypto/md5"
	"encoding/hex"
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
	updates, err := source.GetUpdates(context.Background(), map[string]int64{"old-a": 12, "old-b": 27}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if client.after != 27 || client.limit != 100 {
		t.Fatalf("after=%d limit=%d", client.after, client.limit)
	}
	if got := updates["wr-group@chatroom"]; len(got) != 1 || got[0].RoomID != "wr-group@chatroom" || got[0].Sequence != 30 {
		t.Fatalf("group=%+v", got)
	}
	if got := updates["bob"]; len(got) != 1 || got[0].MsgID != "incoming" {
		t.Fatalf("incoming=%+v", got)
	}
	if got := updates["carol"]; len(got) != 1 || got[0].MsgID != "outgoing" {
		t.Fatalf("outgoing=%+v", got)
	}
}

func TestGetUpdatesUsesInitialSequenceWithoutRooms(t *testing.T) {
	client := &fakeClient{plain: map[string][]byte{}, mediaCalls: map[string]int{}}
	source := newTestSource(t, client, 42)
	if _, err := source.GetUpdates(context.Background(), nil, 100); err != nil {
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
	updates, err := source.GetUpdates(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if calls := client.mediaCalls; !reflect.DeepEqual(calls, map[string]int{"remote-image": 2, "remote-file": 1}) {
		t.Fatalf("media calls=%v", calls)
	}
	imagePath := updates["bob"][0].Image.SDKFileID
	filePath := updates["bob"][1].File.SDKFileID
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
	if _, err := source.GetUpdates(context.Background(), nil, 100); !errors.Is(err, client.chatErr) {
		t.Fatalf("error=%v", err)
	}
}

func TestGetUpdatesReturnsConversionErrors(t *testing.T) {
	client := &fakeClient{
		chatData: []ChatData{{Sequence: 1, MessageID: "voice"}},
		plain: map[string][]byte{
			"voice": []byte(`{"msgid":"voice","action":"send","from":"bob","tolist":["alice"],"msgtime":1,"msgtype":"voice","voice":{}}`),
		},
		mediaCalls: map[string]int{},
	}
	source := newTestSource(t, client, 0)
	if _, err := source.GetUpdates(context.Background(), nil, 100); err == nil {
		t.Fatal("unsupported message was silently skipped")
	}
}

func TestGetUpdatesHonorsCancelledContextBeforeCallingSDK(t *testing.T) {
	client := &fakeClient{plain: map[string][]byte{}, mediaCalls: map[string]int{}}
	source := newTestSource(t, client, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.GetUpdates(ctx, nil, 100); !errors.Is(err, context.Canceled) {
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

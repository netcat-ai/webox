package wechatdb

import "testing"

func TestResolveRecipientDoesNotSpecialCaseFileHelper(t *testing.T) {
	store, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	recipient, err := store.ResolveRecipient("filehelper", "wxid-self")
	if err != nil {
		t.Fatal(err)
	}
	if recipient != nil {
		t.Fatalf("filehelper unexpectedly resolved without a local contact record: %#v", recipient)
	}
}

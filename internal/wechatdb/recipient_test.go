package wechatdb

import "testing"

func TestResolveRecipientDoesNotSpecialCaseFileHelper(t *testing.T) {
	recipient, err := ResolveRecipient(t.TempDir(), nil, t.TempDir(), "filehelper", "wxid-self")
	if err != nil {
		t.Fatal(err)
	}
	if recipient != nil {
		t.Fatalf("filehelper unexpectedly resolved without a local contact record: %#v", recipient)
	}
}
